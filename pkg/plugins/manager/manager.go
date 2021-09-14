package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/plugins/backendplugin"
	"github.com/grafana/grafana/pkg/plugins/backendplugin/instrumentation"
	"github.com/grafana/grafana/pkg/plugins/manager/installer"
	"github.com/grafana/grafana/pkg/plugins/manager/loader"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util/errutil"
	"github.com/grafana/grafana/pkg/util/proxyutil"
)

const (
	grafanaComURL = "https://grafana.com/api/plugins"
)

var _ plugins.Client = (*PluginManager)(nil)

type PluginManager struct {
	Cfg              *setting.Cfg
	license          models.Licensing
	requestValidator models.PluginRequestValidator
	sqlStore         *sqlstore.SQLStore

	plugins         map[string]*plugins.PluginV2
	pluginInstaller installer.Installer
	pluginLoader    loader.Loader
	pluginsMu       sync.RWMutex
	log             log.Logger
}

func ProvideService(cfg *setting.Cfg, license models.Licensing, requestValidator models.PluginRequestValidator,
	sqlStore *sqlstore.SQLStore) (*PluginManager, error) {
	pm := newManager(cfg, license, requestValidator, sqlStore)
	if err := pm.init(); err != nil {
		return nil, err
	}
	return pm, nil
}

func newManager(cfg *setting.Cfg, license models.Licensing, pluginRequestValidator models.PluginRequestValidator,
	sqlStore *sqlstore.SQLStore) *PluginManager {
	return &PluginManager{
		Cfg:              cfg,
		license:          license,
		requestValidator: pluginRequestValidator,
		sqlStore:         sqlStore,
		plugins:          map[string]*plugins.PluginV2{},
		log:              log.New("plugin.manager.v2"),
		pluginInstaller:  installer.New(false, cfg.BuildVersion, newInstallerLogger("plugin.installer", true)),
		pluginLoader:     loader.New(nil, nil, cfg),
	}
}

func (m *PluginManager) init() error {
	// install Core plugins
	err := m.loadPlugins(m.corePluginDirs()...) // wording
	if err != nil {
		return err
	}

	// install Bundled plugins
	err = m.loadPlugins(m.Cfg.BundledPluginsPath)
	if err != nil {
		return err
	}

	// install External plugins
	err = m.loadPlugins(m.Cfg.PluginsPath)
	if err != nil {
		return err
	}

	return nil
}

func (m *PluginManager) Run(ctx context.Context) error {
	<-ctx.Done()
	m.stop(ctx)
	return ctx.Err()
}

func (m *PluginManager) loadPlugins(path ...string) error {
	// think about state + their transitions
	loadedPlugins, err := m.pluginLoader.LoadAll(path, m.registeredPlugins())
	if err != nil {
		return err
	}

	for _, p := range loadedPlugins {
		if err := m.registerAndStart(context.Background(), p); err != nil {
			return err
		}
	}

	return nil
}

func (m *PluginManager) registeredPlugins() map[string]struct{} {
	pluginsByID := make(map[string]struct{})

	m.pluginsMu.RLock()
	defer m.pluginsMu.RUnlock()
	for _, p := range m.plugins {
		pluginsByID[p.ID] = struct{}{}
	}

	return pluginsByID
}

func (m *PluginManager) Plugin(pluginID string) *plugins.PluginV2 {
	m.pluginsMu.RLock()
	p, ok := m.plugins[pluginID]
	m.pluginsMu.RUnlock()

	if ok && (p.IsDecommissioned()) {
		return nil
	}

	return p
}

func (m *PluginManager) PluginByType(pluginID string, pluginType plugins.PluginType) *plugins.PluginV2 {
	m.pluginsMu.RLock()
	p, ok := m.plugins[pluginID]
	m.pluginsMu.RUnlock()

	if ok && (p.IsDecommissioned() || p.Type != pluginType) {
		return nil
	}

	return p
}

func (m *PluginManager) Plugins(pluginTypes ...plugins.PluginType) []*plugins.PluginV2 {
	// if no types passed, assume all
	if len(pluginTypes) == 0 {
		pluginTypes = plugins.PluginTypes
	}

	var requestedTypes = make(map[plugins.PluginType]struct{})
	for _, pt := range pluginTypes {
		requestedTypes[pt] = struct{}{}
	}

	m.pluginsMu.RLock()
	var pluginsList []*plugins.PluginV2
	for _, p := range m.plugins {
		if _, exists := requestedTypes[p.Type]; exists {
			pluginsList = append(pluginsList, p)
		}
	}
	m.pluginsMu.RUnlock()
	return pluginsList
}

func (m *PluginManager) Renderer() *plugins.PluginV2 {
	for _, p := range m.plugins {
		if p.IsRenderer() {
			return p
		}
	}

	return nil
}

func (m *PluginManager) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	plugin := m.Plugin(req.PluginContext.PluginID)
	if plugin == nil {
		return &backend.QueryDataResponse{}, nil
	}

	var resp *backend.QueryDataResponse
	err := instrumentation.InstrumentQueryDataRequest(req.PluginContext.PluginID, func() (innerErr error) {
		resp, innerErr = plugin.QueryData(ctx, req)
		return
	})

	if err != nil {
		if errors.Is(err, backendplugin.ErrMethodNotImplemented) {
			return nil, err
		}

		if errors.Is(err, backendplugin.ErrPluginUnavailable) {
			return nil, err
		}

		return nil, errutil.Wrap("failed to query data", err)
	}

	return resp, err
}

func (m *PluginManager) CallResource(pCtx backend.PluginContext, reqCtx *models.ReqContext, path string) {
	var dsURL string
	if pCtx.DataSourceInstanceSettings != nil {
		dsURL = pCtx.DataSourceInstanceSettings.URL
	}

	err := m.requestValidator.Validate(dsURL, reqCtx.Req)
	if err != nil {
		reqCtx.JsonApiErr(http.StatusForbidden, "Access denied", err)
		return
	}

	clonedReq := reqCtx.Req.Clone(reqCtx.Req.Context())
	rawURL := path
	if clonedReq.URL.RawQuery != "" {
		rawURL += "?" + clonedReq.URL.RawQuery
	}
	urlPath, err := url.Parse(rawURL)
	if err != nil {
		handleCallResourceError(err, reqCtx)
		return
	}
	clonedReq.URL = urlPath
	err = m.callResourceInternal(reqCtx.Resp, clonedReq, pCtx)
	if err != nil {
		handleCallResourceError(err, reqCtx)
	}
}

func (m *PluginManager) callResourceInternal(w http.ResponseWriter, req *http.Request, pCtx backend.PluginContext) error {
	p := m.Plugin(pCtx.PluginID)
	if p == nil {
		return backendplugin.ErrPluginNotRegistered
	}

	keepCookieModel := keepCookiesJSONModel{}
	if dis := pCtx.DataSourceInstanceSettings; dis != nil {
		err := json.Unmarshal(dis.JSONData, &keepCookieModel)
		if err != nil {
			p.Logger().Error("Failed to to unpack JSONData in datasource instance settings", "error", err)
		}
	}

	proxyutil.ClearCookieHeader(req, keepCookieModel.KeepCookies)
	proxyutil.PrepareProxyRequest(req)

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	crReq := &backend.CallResourceRequest{
		PluginContext: pCtx,
		Path:          req.URL.Path,
		Method:        req.Method,
		URL:           req.URL.String(),
		Headers:       req.Header,
		Body:          body,
	}

	return instrumentation.InstrumentCallResourceRequest(p.PluginID(), func() error {
		childCtx, cancel := context.WithCancel(req.Context())
		defer cancel()
		stream := newCallResourceResponseStream(childCtx)

		var wg sync.WaitGroup
		wg.Add(1)

		defer func() {
			if err := stream.Close(); err != nil {
				m.log.Warn("Failed to close stream", "err", err)
			}
			wg.Wait()
		}()

		var flushStreamErr error
		go func() {
			flushStreamErr = flushStream(p, stream, w)
			wg.Done()
		}()

		if err := p.CallResource(req.Context(), crReq, stream); err != nil {
			return err
		}

		return flushStreamErr
	})
}

func handleCallResourceError(err error, reqCtx *models.ReqContext) {
	if errors.Is(err, backendplugin.ErrPluginUnavailable) {
		reqCtx.JsonApiErr(503, "Plugin unavailable", err)
		return
	}

	if errors.Is(err, backendplugin.ErrMethodNotImplemented) {
		reqCtx.JsonApiErr(404, "Not found", err)
		return
	}

	reqCtx.JsonApiErr(500, "Failed to call resource", err)
}

func flushStream(plugin backendplugin.Plugin, stream callResourceClientResponseStream, w http.ResponseWriter) error {
	processedStreams := 0

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if processedStreams == 0 {
				return errors.New("received empty resource response")
			}
			return nil
		}
		if err != nil {
			if processedStreams == 0 {
				return errutil.Wrap("failed to receive response from resource call", err)
			}

			plugin.Logger().Error("Failed to receive response from resource call", "error", err)
			return stream.Close()
		}

		// Expected that headers and status are only part of first stream
		if processedStreams == 0 && resp.Headers != nil {
			// Make sure a content type always is returned in response
			if _, exists := resp.Headers["Content-Type"]; !exists {
				resp.Headers["Content-Type"] = []string{"application/json"}
			}

			for k, values := range resp.Headers {
				// Due to security reasons we don't want to forward
				// cookies from a backend plugin to clients/browsers.
				if k == "Set-Cookie" {
					continue
				}

				for _, v := range values {
					// TODO: Figure out if we should use Set here instead
					// nolint:gocritic
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.Status)
		}

		if _, err := w.Write(resp.Body); err != nil {
			plugin.Logger().Error("Failed to write resource response", "error", err)
		}

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		processedStreams++
	}
}

func (m *PluginManager) CollectMetrics(ctx context.Context, pluginID string) (*backend.CollectMetricsResult, error) {
	p := m.Plugin(pluginID)
	if p == nil {
		return nil, backendplugin.ErrPluginNotRegistered
	}

	var resp *backend.CollectMetricsResult
	err := instrumentation.InstrumentCollectMetrics(p.PluginID(), func() (innerErr error) {
		resp, innerErr = p.CollectMetrics(ctx)
		return
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (m *PluginManager) CheckHealth(ctx context.Context, pluginContext backend.PluginContext) (*backend.CheckHealthResult, error) {
	var dsURL string
	if pluginContext.DataSourceInstanceSettings != nil {
		dsURL = pluginContext.DataSourceInstanceSettings.URL
	}

	err := m.requestValidator.Validate(dsURL, nil)
	if err != nil {
		return &backend.CheckHealthResult{
			Status:  http.StatusForbidden,
			Message: "Access denied",
		}, nil
	}

	p := m.Plugin(pluginContext.PluginID)
	if p == nil {
		return nil, backendplugin.ErrPluginNotRegistered
	}

	var resp *backend.CheckHealthResult
	err = instrumentation.InstrumentCheckHealthRequest(p.PluginID(), func() (innerErr error) {
		resp, innerErr = p.CheckHealth(ctx, &backend.CheckHealthRequest{PluginContext: pluginContext})
		return
	})

	if err != nil {
		if errors.Is(err, backendplugin.ErrMethodNotImplemented) {
			return nil, err
		}

		if errors.Is(err, backendplugin.ErrPluginUnavailable) {
			return nil, err
		}

		return nil, errutil.Wrap("failed to check plugin health", backendplugin.ErrHealthCheckFailed)
	}

	return resp, nil
}

func (m *PluginManager) isRegistered(pluginID string) bool {
	p := m.Plugin(pluginID)
	if p == nil {
		return false
	}

	return !p.IsDecommissioned()
}

func (m *PluginManager) register(p *plugins.PluginV2) error {
	m.pluginsMu.Lock()
	defer m.pluginsMu.Unlock()

	pluginID := p.ID
	if _, exists := m.plugins[pluginID]; exists {
		return fmt.Errorf("plugin %s already registered", pluginID)
	}

	m.plugins[pluginID] = p
	m.log.Debug("Plugin registered", "pluginId", pluginID)
	return nil
}

func (m *PluginManager) registerAndStart(ctx context.Context, plugin *plugins.PluginV2) error {
	err := m.register(plugin)
	if err != nil {
		return err
	}

	if !m.isRegistered(plugin.ID) {
		return fmt.Errorf("plugin %s is not registered", plugin.ID)
	}

	m.start(ctx, plugin)

	return nil
}

func (m *PluginManager) unregisterAndStop(ctx context.Context, p *plugins.PluginV2) error {
	m.log.Debug("Stopping plugin process", "pluginId", p.ID)
	if err := p.Decommission(); err != nil {
		return err
	}

	if err := p.Stop(ctx); err != nil {
		return err
	}

	delete(m.plugins, p.ID)

	m.log.Debug("Plugin unregistered", "pluginId", p.ID)
	return nil
}

func (m *PluginManager) Install(ctx context.Context, pluginID, version string, opts plugins.InstallOpts) error {
	var pluginZipURL string

	if opts.PluginRepoURL == "" {
		opts.PluginRepoURL = grafanaComURL
	}

	plugin := m.Plugin(pluginID)
	if plugin != nil {
		if !plugin.IsExternalPlugin() {
			return plugins.ErrInstallCorePlugin
		}

		if plugin.Info.Version == version {
			return plugins.DuplicatePluginError{
				PluginID:          plugin.ID,
				ExistingPluginDir: plugin.PluginDir,
			}
		}

		// get plugin update information to confirm if upgrading is possible
		updateInfo, err := m.pluginInstaller.GetUpdateInfo(pluginID, version, opts.PluginRepoURL)
		if err != nil {
			return err
		}

		pluginZipURL = updateInfo.PluginZipURL

		// remove existing installation of plugin
		err = m.Uninstall(ctx, plugin.ID)
		if err != nil {
			return err
		}
	}

	if opts.InstallDir == "" {
		opts.InstallDir = m.Cfg.PluginsPath
	}

	if opts.PluginZipURL == "" {
		opts.PluginZipURL = pluginZipURL
	}

	err := m.pluginInstaller.Install(ctx, pluginID, version, opts.InstallDir, opts.PluginZipURL, opts.PluginRepoURL)
	if err != nil {
		return err
	}

	err = m.loadPlugins(opts.InstallDir)
	if err != nil {
		return err
	}

	return nil
}

func (m *PluginManager) Uninstall(ctx context.Context, pluginID string) error {
	plugin := m.Plugin(pluginID)
	if plugin == nil {
		return plugins.ErrPluginNotInstalled
	}

	if !plugin.IsExternalPlugin() {
		return plugins.ErrUninstallCorePlugin
	}

	// extra security check to ensure we only remove plugins that are located in the configured plugins directory
	path, err := filepath.Rel(m.Cfg.PluginsPath, plugin.PluginDir)
	if err != nil || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return plugins.ErrUninstallOutsideOfPluginDir
	}

	if m.isRegistered(pluginID) {
		err := m.unregisterAndStop(ctx, plugin)
		if err != nil {
			return err
		}
	}

	return m.pluginInstaller.Uninstall(ctx, plugin.PluginDir)
}

// start starts all managed backend plugins
func (m *PluginManager) start(ctx context.Context, p *plugins.PluginV2) {
	if !p.IsManaged() || !p.Backend {
		return
	}

	if err := startPluginAndRestartKilledProcesses(ctx, p); err != nil {
		p.Logger().Error("Failed to start plugin", "error", err)
	}
}

func (m *PluginManager) stop(ctx context.Context) {
	m.pluginsMu.RLock()
	defer m.pluginsMu.RUnlock()
	var wg sync.WaitGroup
	for _, p := range m.plugins {
		wg.Add(1)
		go func(p backendplugin.Plugin, ctx context.Context) {
			defer wg.Done()
			p.Logger().Debug("Stopping plugin")
			if err := p.Stop(ctx); err != nil {
				p.Logger().Error("Failed to stop plugin", "error", err)
			}
			p.Logger().Debug("Plugin stopped")
		}(p, ctx)
	}
	wg.Wait()
}

func (m *PluginManager) Register(pluginID string, factory backendplugin.PluginFactoryFunc) error {
	if m.isRegistered(pluginID) {
		return fmt.Errorf("backend plugin %s already registered", pluginID)
	}

	path := filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource", pluginID)

	p, err := m.pluginLoader.LoadWithFactory(path, factory)
	if err != nil {
		return err
	}

	err = m.register(p)
	if err != nil {
		return err
	}

	return nil
}

func (m *PluginManager) StaticRoutes() []*plugins.PluginStaticRoute {
	var staticRoutes []*plugins.PluginStaticRoute

	for _, p := range m.Plugins() {
		staticRoutes = append(staticRoutes, p.StaticRoute())
	}
	return staticRoutes
}

func (m *PluginManager) corePluginDirs() []string {
	datasourcePaths := []string{
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/cloud-monitoring"),
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/alertmanager"),
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/dashboard"),
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/jaeger"),
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/mixed"),
		filepath.Join(m.Cfg.StaticRootPath, "app/plugins/datasource/zipkin"),
	}

	panelsPath := filepath.Join(m.Cfg.StaticRootPath, "app/plugins/panel")

	return append(datasourcePaths, panelsPath)
}

func startPluginAndRestartKilledProcesses(ctx context.Context, p *plugins.PluginV2) error {
	if err := p.Start(ctx); err != nil {
		return err
	}

	go func(ctx context.Context, p *plugins.PluginV2) {
		if err := restartKilledProcess(ctx, p); err != nil {
			p.Logger().Error("Attempt to restart killed plugin process failed", "error", err)
		}
	}(ctx, p)

	return nil
}

func restartKilledProcess(ctx context.Context, p *plugins.PluginV2) error {
	ticker := time.NewTicker(time.Second * 1)

	for {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		case <-ticker.C:
			if p.IsDecommissioned() {
				p.Logger().Debug("Plugin decommissioned")
				return nil
			}

			if !p.Exited() {
				continue
			}

			p.Logger().Debug("Restarting plugin")
			if err := p.Start(ctx); err != nil {
				p.Logger().Error("Failed to restart plugin", "error", err)
				continue
			}
			p.Logger().Debug("Plugin restarted")
		}
	}
}

// callResourceClientResponseStream is used for receiving resource call responses.
type callResourceClientResponseStream interface {
	Recv() (*backend.CallResourceResponse, error)
	Close() error
}

type keepCookiesJSONModel struct {
	KeepCookies []string `json:"keepCookies"`
}

type callResourceResponseStream struct {
	ctx    context.Context
	stream chan *backend.CallResourceResponse
	closed bool
}

func newCallResourceResponseStream(ctx context.Context) *callResourceResponseStream {
	return &callResourceResponseStream{
		ctx:    ctx,
		stream: make(chan *backend.CallResourceResponse),
	}
}

func (s *callResourceResponseStream) Send(res *backend.CallResourceResponse) error {
	if s.closed {
		return errors.New("cannot send to a closed stream")
	}

	select {
	case <-s.ctx.Done():
		return errors.New("cancelled")
	case s.stream <- res:
		return nil
	}
}

func (s *callResourceResponseStream) Recv() (*backend.CallResourceResponse, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case res, ok := <-s.stream:
		if !ok {
			return nil, io.EOF
		}
		return res, nil
	}
}

func (s *callResourceResponseStream) Close() error {
	if s.closed {
		return errors.New("cannot close a closed stream")
	}

	close(s.stream)
	s.closed = true
	return nil
}
