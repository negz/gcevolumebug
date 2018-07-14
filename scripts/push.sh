#!/usr/bin/env bash

set -e

VERSION=$(git rev-parse --short HEAD)
docker push "negz/gvb:latest"
docker push "negz/gvb:${VERSION}"
