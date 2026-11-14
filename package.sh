#!/bin/sh
# Builds dist/ecc-server_<version>_amd64.deb. Needs Go, curl, GNU tar and sha256sum. An argument overrides VERSION (CI
# passes <VERSION>~dev.<run>).
#
# The package carries the five OpenVINO shared libraries the models run on, from OpenVINO's release archive: the
# release the bundles are exported with, checked against its published SHA-256. The archive is kept in dist/ for the
# next build.
set -eu
cd "$(dirname "$0")"
version=${1:-$(tr -d '[:space:]' < VERSION)}

ov_release=2026.4.1
ov_archive=openvino_toolkit_ubuntu24_2026.4.1.22982.07f9c262b05_x86_64.tgz
ov_sha256=772ce52d9b0aa375c77d391fccea71450d889c98b9af197bb8f05167a9dabd87
ov_url=https://storage.openvinotoolkit.org/repositories/openvino/packages/$ov_release/linux/$ov_archive

mkdir -p dist
if ! echo "$ov_sha256  dist/$ov_archive" | sha256sum -c --status 2>/dev/null; then
  echo "fetching $ov_url"
  curl -fsSL --retry 3 -o "dist/$ov_archive.part" "$ov_url"
  if ! echo "$ov_sha256  dist/$ov_archive.part" | sha256sum -c --status; then
    rm -f "dist/$ov_archive.part"
    echo "the OpenVINO archive does not match its SHA-256 $ov_sha256" >&2
    exit 1
  fi
  mv "dist/$ov_archive.part" "dist/$ov_archive"
fi
rm -rf dist/openvino
mkdir -p dist/openvino
tar -xzf "dist/$ov_archive" -C dist/openvino --wildcards --transform='s|.*/||' \
  "*/runtime/lib/intel64/libopenvino.so.$ov_release" \
  "*/runtime/lib/intel64/libopenvino_c.so.$ov_release" \
  '*/runtime/lib/intel64/libopenvino_intel_cpu_plugin.so' \
  "*/runtime/lib/intel64/libopenvino_ir_frontend.so.$ov_release" \
  '*/runtime/3rdparty/tbb/lib/libtbb.so.12.13'
[ "$(ls dist/openvino | wc -l)" = 5 ] || { echo "dist/openvino should hold 5 libraries: $(ls dist/openvino)" >&2; exit 1; }

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$version" -o dist/ecc-server .
deb="dist/ecc-server_${version}_amd64.deb"
VERSION=$version go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.49.0 package --packager deb --config nfpm.yaml --target "$deb"
echo "built $deb"
echo "publish with: telesphorepo publish $deb"
