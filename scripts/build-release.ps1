$ErrorActionPreference = 'Stop'
$Root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $Root
New-Item -ItemType Directory -Force -Path 'dist/linux-amd64', 'dist/windows-amd64' | Out-Null

if (-not $env:WEATHERNEXT_OAUTH_CLIENT_ID -or -not $env:WEATHERNEXT_OAUTH_CLIENT_SECRET) {
    throw 'Set WEATHERNEXT_OAUTH_CLIENT_ID and WEATHERNEXT_OAUTH_CLIENT_SECRET before building release binaries.'
}
go mod download
$ObfuscatedSecret = (& go run .\scripts\obfuscate-secret.go).Trim()
if ($LASTEXITCODE -ne 0 -or $ObfuscatedSecret -notmatch '^([0-9a-f]+):([0-9a-f]+)$') {
    throw 'Could not XOR-obfuscate the OAuth client secret for this build.'
}
$SecretCipher = $Matches[1]
$SecretKey = $Matches[2]
Remove-Item Env:WEATHERNEXT_OAUTH_CLIENT_SECRET
$LdFlags = "-s -w -X weathernext/internal/backend.OAuthClientID=$env:WEATHERNEXT_OAUTH_CLIENT_ID -X weathernext/internal/backend.OAuthClientSecretCipher=$SecretCipher -X weathernext/internal/backend.OAuthClientSecretKey=$SecretKey"
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags="$LdFlags" -o 'dist/linux-amd64/weathernext' ./cmd/weathernext

$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
go build -trimpath -ldflags="$LdFlags" -o 'dist/windows-amd64/weathernext.exe' ./cmd/weathernext

Write-Host 'Built dist/linux-amd64/weathernext and dist/windows-amd64/weathernext.exe'
