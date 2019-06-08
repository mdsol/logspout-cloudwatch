#!/bin/sh
set -ex
apk add --update go build-base git mercurial ca-certificates
mkdir -p /go/src/github.com/gliderlabs
cp -r /src /go/src/github.com/gliderlabs/logspout
cd /go/src/github.com/gliderlabs/logspout
export GOPATH=/go
go get -x 
go build -ldflags "-X main.version=$1" -o /bin/logspout
apk del go git mercurial build-base
rm -rf /go /var/cache/apk/* /root/.glide

# backwards compatability
ln -fs /tmp/docker.sock /var/run/docker.sock
