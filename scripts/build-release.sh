#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
mkdir -p dist/linux-amd64 dist/windows-amd64

if [ -z "${WEATHERNEXT_OAUTH_CLIENT_ID:-}" ] || [ -z "${WEATHERNEXT_OAUTH_CLIENT_SECRET:-}" ]; then
  echo 'Set WEATHERNEXT_OAUTH_CLIENT_ID and WEATHERNEXT_OAUTH_CLIENT_SECRET before building release binaries.' >&2
  exit 1
fi

go mod download
obfuscated_secret=$(go run ./scripts/obfuscate-secret.go)
secret_cipher=${obfuscated_secret%%:*}
secret_key=${obfuscated_secret#*:}
unset obfuscated_secret WEATHERNEXT_OAUTH_CLIENT_SECRET
ldflags='-s -w'
ldflags="$ldflags -X weathernext/internal/backend.OAuthClientID=$WEATHERNEXT_OAUTH_CLIENT_ID"
ldflags="$ldflags -X weathernext/internal/backend.OAuthClientSecretCipher=$secret_cipher"
ldflags="$ldflags -X weathernext/internal/backend.OAuthClientSecretKey=$secret_key"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$ldflags" -o dist/linux-amd64/weathernext ./cmd/weathernext
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$ldflags" -o dist/windows-amd64/weathernext.exe ./cmd/weathernext

printf 'Built dist/linux-amd64/weathernext and dist/windows-amd64/weathernext.exe\n'
