# WeatherNext 3 terminal viewer

Charm-based interactive terminal UI for browsing WeatherNext forecast JSON and
querying the Python worker. This repository contains the Go application only;
it deliberately excludes the Python client, credentials, saved forecasts, and
local configuration.

## Requirements

- Go 1.26 or newer (the Charm v2 dependencies require Go 1.26).
- A checkout of the WeatherNext Python project containing `weathercore/worker.py`
  and its configured virtual environment. The TUI discovers it in a parent
  directory, or you can set `WEATHERNEXT_HOME` to that project's path.
- Google Cloud credentials for BigQuery queries. Forecast viewing from a saved
  JSON file works offline.

## Build Linux and Windows x64

From this repository root, run:

```sh
./scripts/build-release.sh
```

On Windows PowerShell:

```powershell
.\scripts\build-release.ps1
```

The scripts download Go modules and create:

- `dist/linux-amd64/weathernext`
- `dist/windows-amd64/weathernext.exe`

Run the binary with the Python project directory as the working directory so
`output/forecast.json` opens by default, or pass `--home <project-folder>` to
select the worker project. Use `--input <path>` for a different forecast file.

```sh
/path/to/weathernext-tui/dist/linux-amd64/weathernext
```

```powershell
& 'C:\path\to\weathernext-tui\dist\windows-amd64\weathernext.exe'
```

## Development

```sh
go test ./...
go run ./cmd/weathernext --print --input /path/to/forecast.json
```
