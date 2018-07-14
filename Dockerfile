FROM golang:1.10-stretch AS build
WORKDIR /go/src/github.com/negz/gcevolumebug
COPY . .
RUN go get -u github.com/golang/dep/cmd/dep
RUN dep ensure
RUN go build -o /gvb .

FROM debian:stretch-slim
RUN apt-get update && apt-get install -y --no-install-recommends sysstat systemd && apt-get clean
COPY --from=build /gvb /gvb