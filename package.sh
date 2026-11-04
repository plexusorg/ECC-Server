#!/bin/sh
# Builds dist/ecc-server_<version>_amd64.deb. Needs Go. An argument overrides VERSION (CI passes <VERSION>~dev.<run>).
set -eu
cd "$(dirname "$0")"
version=${1:-$(tr -d '[:space:]' < VERSION)}
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$version" -o dist/ecc-server .
deb="dist/ecc-server_${version}_amd64.deb"
VERSION=$version go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0 package --packager deb --config nfpm.yaml --target "$deb"
echo "built $deb"
echo "publish with: telesphorepo publish $deb"
