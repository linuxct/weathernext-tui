$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $Root
New-Item -ItemType Directory -Force -Path 'dist/linux-amd64', 'dist/windows-amd64' | Out-Null

go mod download
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags='-s -w' -o 'dist/linux-amd64/weathernext' ./cmd/weathernext

$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags='-s -w' -o 'dist/windows-amd64/weathernext.exe' ./cmd/weathernext

Write-Host 'Built dist/linux-amd64/weathernext and dist/windows-amd64/weathernext.exe'
