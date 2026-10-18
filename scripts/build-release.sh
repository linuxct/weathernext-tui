#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
mkdir -p dist/linux-amd64 dist/windows-amd64

go mod download
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/linux-amd64/weathernext ./cmd/weathernext
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/windows-amd64/weathernext.exe ./cmd/weathernext

printf 'Built dist/linux-amd64/weathernext and dist/windows-amd64/weathernext.exe\n'
