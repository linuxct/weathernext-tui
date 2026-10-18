# WeatherNext 3 terminal viewer

Charm-based, full-screen terminal UI for WeatherNext 3. The application is
implemented in Go and calls Google's OAuth, BigQuery, Service Usage, API Keys,
and Geocoding APIs directly. Runtime use does not require Python, `gcloud`, or
the Google Cloud SDK. Account credentials and local application state stay on
the user's computer, outside the repository.

The app uses a full-screen, pane-based layout with resize-aware wrapping.
On first launch it completes Google sign-in before opening any saved forecast;
then it asks for the user's target project ID, dataset, and table.

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

In the TUI, enter the Project ID, Dataset ID, and Table ID into their separate
fields. These IDs are supplied explicitly because a Google account does not
identify which shared dataset you subscribed to. Press **Ctrl+T** to check table
metadata without running a forecast query. Forecast refreshes can incur
BigQuery charges, so configure a maximum bytes billed in Settings before
pressing `r`.

The app reads the dataset's region automatically. After target access is
verified, it also tries to enable the API Keys and Geocoding APIs and create a
Geocoding-restricted Maps key. If that setup is denied, the signed-in account
needs Service Usage Admin and API Keys Admin on the target project, and that
project must have billing enabled. Location search then works without asking
the user to supply a Maps key.

### 5. Configure the app's OAuth client (one-time maintainer setup)

This registers the application once. Users of the resulting binary only sign
in with Google, then enter their own WeatherNext project, dataset, and table.

1. In Cloud Console, select the project used for this app, then open **Google
   Auth Platform → Branding** and register the app with a name, support email,
   and developer contact email.
2. Open **Audience**. For a personal test build, choose **External**, leave the
   publishing status as **Testing**, and add your Google account as a test user.
   Testing authorizations expire after seven days. Long-term distribution may
   require publishing and Google's OAuth verification for the requested scope.
3. Open **Data Access** and add
   `https://www.googleapis.com/auth/cloud-platform`.
4. Open **Clients → Create client**, choose **Desktop app**, create it, and copy
   its **Client ID** and **Client Secret**. Configure this exact pair together;
   Google's token endpoint for the OAuth client used by this app currently
   rejects exchanges without the secret.
5. In the GitHub repository, open **Settings → Secrets and variables → Actions**
   and add `WEATHERNEXT_OAUTH_CLIENT_ID` as a repository variable (recommended)
   or secret, and add `WEATHERNEXT_OAUTH_CLIENT_SECRET` as a repository secret.
   The build workflow XOR-obfuscates the secret before embedding it, so its
   plaintext does not appear in the binary's string table. The key and decoder
   are also in the binary, so this only blocks casual string searches; anyone
   with a binary can recover the secret. For public distribution, use an OAuth
   client intended for installed desktop apps and treat its embedded secret as
   public.

Google's [OAuth client guide](https://support.google.com/cloud/answer/15549257)
and [app audience guide](https://support.google.com/cloud/answer/15549945)
describe these settings and the seven-day testing limit.

## Requirements

- Released binaries run on Linux and Windows without Python, `gcloud`, or a
  preinstalled Google SDK. Builds from source require Go 1.26 or newer.
- Interactive sign-in uses the app's embedded OAuth client ID and XOR-obfuscated
  client secret. It opens the system browser and checks that Google sign-in can
  still refresh before opening any saved forecast. User refresh credentials are
  saved separately in the user's configuration directory. The app does not
  inspect or use `gcloud`/ADC credentials, so the user chooses the account.
- Each user enters the target project ID, WeatherNext dataset, and table in
  Settings. The app verifies table metadata before any forecast query. That
  check does not run a billed forecast query. Static `--print` output works
  offline.

## Build Linux and Windows x64

From this repository root, run:

```sh
WEATHERNEXT_OAUTH_CLIENT_ID='your-client-id.apps.googleusercontent.com' \
WEATHERNEXT_OAUTH_CLIENT_SECRET='your-client-secret' \
./scripts/build-release.sh
```

On Windows PowerShell:

```powershell
$env:WEATHERNEXT_OAUTH_CLIENT_ID = 'your-client-id.apps.googleusercontent.com'
$env:WEATHERNEXT_OAUTH_CLIENT_SECRET = 'your-client-secret'
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

Set `WEATHERNEXT_OAUTH_CLIENT_ID` as a repository Actions variable or secret,
and `WEATHERNEXT_OAUTH_CLIENT_SECRET` as a repository Actions secret. Each
binary gets a fresh random XOR key and is uploaded as its own downloadable
artifact, retained for 30 days. To download one, open the repository's
**Actions** tab, select the
**Build binaries** run, then choose the platform artifact in the run summary's
**Artifacts** section. The artifacts contain the binary directly, not a zip
file. GitHub documents the artifact download section in the
[`upload-artifact` action](https://github.com/actions/upload-artifact#where-does-the-upload-go).

After Google sign-in and table setup, the app opens a local saved forecast when
available. Pass `--home <folder>` to choose another folder for forecast files
and application state, or `--input <path>` to open a forecast JSON file.
Removed places are not restored by reopening an old cached JSON.

```sh
/path/to/weathernext-tui/dist/linux-amd64/weathernext
```

```powershell
& 'C:\path\to\weathernext-tui\dist\windows-amd64\weathernext.exe'
```

## Development

```sh
go test ./...
WEATHERNEXT_OAUTH_CLIENT_ID='your-client-id.apps.googleusercontent.com' \
WEATHERNEXT_OAUTH_CLIENT_SECRET='your-client-secret' go run ./cmd/weathernext
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
