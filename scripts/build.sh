#!/usr/bin/env bash

VERSION=$(git rev-parse --short HEAD)
docker build --tag "negz/gvb:latest" .
docker build --tag "negz/gvb:${VERSION}" .
