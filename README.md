# WeatherNext 3 terminal viewer

Charm-based interactive terminal UI for browsing WeatherNext forecast JSON and
querying the Python worker. This repository contains the Go application only;
it deliberately excludes the Python client, credentials, saved forecasts, and
local configuration.

The app uses a full-screen, pane-based layout with resize-aware wrapping.

## Google Cloud setup

Complete these steps with the same Google Account that was approved for
WeatherNext 3 access. WeatherNext access is granted to an account; creating a
Cloud project does not itself grant access to the listing. See Google's
[WeatherNext access guide](https://developers.google.com/weathernext/guides/access-forecast).

### 1. Create a project and attach billing

1. Open the [Google Cloud Console](https://console.cloud.google.com/) and sign
   in with the approved account.
2. Open the project selector at the top of the page and choose **New Project**.
3. Enter a project name, choose an organization or folder if prompted, and
   create the project. Google generates a globally unique **Project ID**. You
   can change it during creation; after creation it is permanent. Record the
   Project ID exactly as shown. The project name and project number are
   different values.
4. Link an active Cloud Billing account to the project if one was not selected
   during creation. BigQuery query usage is charged to the billing account
   linked to this project. Review Google's guide to
   [project billing](https://docs.cloud.google.com/billing/docs/how-to/modify-project).

Google's [project creation guide](https://docs.cloud.google.com/resource-manager/docs/creating-managing-projects)
has additional details about project IDs and organization permissions.

### 2. Enable the required APIs

Make sure the new project is selected in the Console, then open **APIs &
Services → Library** and enable these APIs:

- **BigQuery API** (`bigquery.googleapis.com`)
- **Analytics Hub API** (`analyticshub.googleapis.com`), also called BigQuery
  sharing in the current Console

The Analytics Hub API is the API needed to discover and subscribe to the
WeatherNext listing. The separate Google Analytics APIs are not needed for this
workflow. Google documents the subscription flow in [View and subscribe to
listings](https://docs.cloud.google.com/bigquery/docs/analytics-hub-view-subscribe-listings).

### 3. Subscribe to the WeatherNext 3 listing

1. In the Console, open **BigQuery → Sharing (Analytics Hub)**. If the menu is
   not visible, search the Console for **Sharing (Analytics Hub)**.
2. Choose **Search listings**, search for **WeatherNext 3**, and open the
   WeatherNext 3 listing. The listing is available only to accounts that have
   been granted access.
3. Choose **Subscribe**. In the linked-dataset dialog, select the new project,
   enter a linked dataset name (for example, `weathernext_3`), choose an
   available primary region, and save.
4. Wait for the linked dataset to appear under the project in BigQuery's
   **Explorer**. It is read-only; subscribing links the shared tables into your
   project rather than copying their data.

See Google's [WeatherNext BigQuery guide](https://developers.google.com/weathernext/guides/bigquery)
for the listing and available tables.

### 4. Copy the three IDs into the TUI

In BigQuery's **Explorer**, expand your project and linked dataset:

- **Project ID:** use the exact Project ID from the project selector or the
  project's **Project info** card. Do not use the display name or project
  number.
- **Dataset ID:** use the linked dataset name you entered when subscribing.
  Click the dataset to verify its ID in the details pane.
- **Table ID:** expand the dataset and use `weathernext_3_0_0_0p1deg` for the
  0.1° gridded forecasts supported by this app. The 0.05° table is a different
  station-head product.

The fully qualified table has this form:

```text
<project-id>.<dataset-id>.weathernext_3_0_0_0p1deg
```

In the TUI, open **Settings** and enter the Project ID, Dataset ID, and Table
ID into their separate fields. A Project ID detected in Application Default
Credentials is only a suggestion; it may not be the project where you created
the linked dataset. Press **Ctrl+T** to check table metadata without running a
forecast query. Forecast refreshes can incur BigQuery charges, so configure a
maximum bytes billed in Settings before pressing `r`.

## Requirements

- Go 1.26 or newer (the Charm v2 dependencies require Go 1.26).
- A checkout of the WeatherNext Python project containing `weathercore/worker.py`
  and its configured virtual environment. The TUI discovers it in a parent
  directory, or you can set `WEATHERNEXT_HOME` to that project's path.
- Google Cloud credentials for interactive use. Startup checks credentials
  before opening saved forecasts. Settings requires an explicit Google Cloud
  project ID, WeatherNext dataset name, and table name; the app verifies table
  metadata before any forecast query. This check is not a billed forecast
  query. A project ID found in ADC credentials is offered as a suggestion only.
  Credentials do not identify a shared WeatherNext dataset or table. Static
  `--print` output works offline.

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

## GitHub Actions builds

Every push runs the Go tests and builds four standalone binaries with CGO
disabled:

- Linux amd64 (x86_64)
- Linux arm64
- Windows amd64 (x86_64)
- Windows arm64

Each binary is uploaded as its own downloadable artifact and retained for 30
days. To download one, open the repository's **Actions** tab, select the
**Build binaries** run, then choose the platform artifact in the run summary's
**Artifacts** section. The artifacts contain the binary directly, not a zip
file. GitHub documents the artifact download section in the
[`upload-artifact` action](https://github.com/actions/upload-artifact#where-does-the-upload-go).

The binary discovers the Python project in its parent folders and opens that
project’s `output/forecast.json`. Pass `--home <project-folder>` to select another
worker project, or `--input <path>` to open another forecast file. Removed places
are not restored by reopening an old cached JSON.

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

## Forecast navigation

- `1` Forecast, `2` Daily, `3` Raw data.
- Left/right selects the six-hour period, local day, or raw forecast hour.
- Up/down scrolls the readable data or raw JSON.
- `h` focuses Locations; up/down selects a place, `d` removes it, and right returns to the forecast.
- `r` refreshes the selected location; `/` searches for another place.
- In Billing, up/down (or `k`/`j`) scrolls the ledger and recent jobs; the pane title shows the visible line range.

Readable Forecast and Daily data show one average per variable over the selected
period or day, calculated from available hourly model medians (P50). If a variable
has no usable medians, available hourly means are used and labeled as a fallback.
Raw data retains every hourly value and percentile. Rain on cards is a total
over the available hours; readable rainfall is labeled as an hourly average.
