#This dockerfile defines a docker image that is used as a build environment by the pipelines

FROM ubuntu:18.04
#####INSTALL CRITICAL STUFF
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update
RUN apt-get -y install curl unzip sudo git dirmngr apt-transport-https lsb-release ca-certificates vim default-jre

#####INSTALL NODE.JS v14
RUN curl -sL https://deb.nodesource.com/setup_14.x | bash - && \
    apt -y install nodejs

#####INSTALL PACKAGE MANAGERS
RUN npm install --global yarn --unsafe-perm
