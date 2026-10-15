#!/bin/sh
# Rebuilds the click-files' binaries from src/: "Lutecia Download.exe" and bin/*. Needs only Docker.
# Builds are reproducible (-trimpath, no build id): unchanged source gives byte-identical files, so git sees no change.
set -eu
cd "$(dirname "$0")"
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e CGO_ENABLED=0 \
  -v "$PWD":/repo -w /repo/src golang:1.25-alpine sh -ec '
    go vet ./... && go test ./...
    b() { GOOS=$1 GOARCH=$2 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$3" .; }
    b windows amd64 "../Lutecia Download.exe"
    b darwin  arm64 ../bin/lutecia-download-darwin-arm64
    b darwin  amd64 ../bin/lutecia-download-darwin-amd64
    b linux   amd64 ../bin/lutecia-download-linux-amd64
    b linux   arm64 ../bin/lutecia-download-linux-arm64'
chmod +x bin/* "Lutecia Download.command"
ls -l "Lutecia Download.exe" bin/
