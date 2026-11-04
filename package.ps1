# Builds dist/ecc-server_<version>_amd64.deb. Needs Go.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
$version = (Get-Content VERSION -Raw).Trim()
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'amd64'
try {
    go build -trimpath -ldflags="-s -w -X main.version=$version" -o dist/ecc-server .
    if ($LASTEXITCODE) { throw 'go build failed' }
} finally {
    $env:CGO_ENABLED = $null; $env:GOOS = $null; $env:GOARCH = $null
}
$env:VERSION = $version
$deb = "dist/ecc-server_${version}_amd64.deb"
go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0 package --packager deb --config nfpm.yaml --target $deb
if ($LASTEXITCODE) { throw 'nfpm failed' }
Write-Host "built $deb"
Write-Host "publish with: telesphorepo publish $deb"
