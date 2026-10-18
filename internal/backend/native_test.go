package backend

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"weathernext/internal/protocol"
)

func TestBrowserOAuthIsStoredSeparatelyAndADCIsIgnored(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("WEATHERNEXT_OAUTH_CLIENT_ID", "client.apps.googleusercontent.com")
	adcPath := filepath.Join(config, "unrelated", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(adcPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adcPath, []byte(`{"type":"service_account","project_id":"wrong-project"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adcPath)
	status := newAuthService("").status(context.Background())
	if status["authenticated"] != false || status["source"] != nil {
		t.Fatalf("the app must not silently use ADC: %#v", status)
	}
	if err := saveOAuthCredentials("public-client-id.apps.googleusercontent.com", "refresh-token-value"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(adcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"type":"service_account","project_id":"wrong-project"}` {
		t.Fatal("browser sign-in modified unrelated credentials")
	}
	data, err := os.ReadFile(oauthCredentialPath())
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["client_id"] != "public-client-id.apps.googleusercontent.com" || stored["refresh_token"] != "refresh-token-value" || stored["client_secret"] != "" {
		t.Fatalf("unexpected saved OAuth credentials: %#v", stored)
	}
}

func TestOAuthClientIDCanBeInjectedAtBuildTime(t *testing.T) {
	t.Setenv("WEATHERNEXT_OAUTH_CLIENT_ID", "")
	previous := OAuthClientID
	OAuthClientID = "build-client.apps.googleusercontent.com"
	t.Cleanup(func() { OAuthClientID = previous })
	if got := oauthClientID(); got != "build-client.apps.googleusercontent.com" {
		t.Fatalf("embedded OAuth client ID = %q", got)
	}
}

func TestOAuthClientSecretIsDecodedFromBuildTimeXOR(t *testing.T) {
	t.Setenv("WEATHERNEXT_OAUTH_CLIENT_SECRET", "")
	previousCipher, previousKey := OAuthClientSecretCipher, OAuthClientSecretKey
	secret := []byte("build-secret-value")
	key := []byte("random-test-key-123")
	ciphertext := make([]byte, len(secret))
	for i := range secret {
		ciphertext[i] = secret[i] ^ key[i%len(key)]
	}
	OAuthClientSecretCipher = hex.EncodeToString(ciphertext)
	OAuthClientSecretKey = hex.EncodeToString(key)
	t.Cleanup(func() {
		OAuthClientSecretCipher, OAuthClientSecretKey = previousCipher, previousKey
	})
	if got := oauthClientSecret(); got != string(secret) {
		t.Fatalf("decoded OAuth client secret = %q", got)
	}
}

func TestLoopbackOAuthSignInUsesClientSecretForExchangeAndRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WEATHERNEXT_OAUTH_CLIENT_ID", "public-client.apps.googleusercontent.com")
	t.Setenv("WEATHERNEXT_OAUTH_CLIENT_SECRET", "test-client-secret")
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_id") != "public-client.apps.googleusercontent.com" {
			t.Errorf("wrong client id: %q", r.Form.Get("client_id"))
		}
		if r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code_verifier") == "" {
			t.Error("PKCE verifier was not sent")
		}
		if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "" {
			t.Error("refresh grant did not include the saved refresh token")
		}
		if r.Form.Get("client_secret") != "test-client-secret" {
			t.Errorf("OAuth token request has wrong client secret: %q", r.Form.Get("client_secret"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access","token_type":"Bearer","expires_in":3600,"refresh_token":"refresh"}`))
	}))
	defer tokenServer.Close()
	auth := &authService{endpoint: oauth2.Endpoint{AuthURL: tokenServer.URL + "/authorize", TokenURL: tokenServer.URL + "/token"}}
	started := auth.start()
	if started["status"] != "started" {
		t.Fatalf("OAuth did not start: %#v", started)
	}
	authURL, err := url.Parse(stringValue(started["verification_url"]))
	if err != nil {
		t.Fatal(err)
	}
	callback := authURL.Query().Get("redirect_uri")
	state := authURL.Query().Get("state")
	if state == "" || authURL.Query().Get("code_challenge") == "" {
		t.Fatalf("state or PKCE challenge missing from auth URL: %s", authURL)
	}
	callbackURL := callback + "?state=" + url.QueryEscape(state) + "&code=sample-code"
	response, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback returned %s", response.Status)
	}
	finished := auth.respond(map[string]any{"request_id": started["request_id"]})
	if finished["status"] != "complete" {
		t.Fatalf("sign-in did not complete: %#v", finished)
	}
	status := auth.status(context.Background())
	if status["authenticated"] != true || status["source"] != "WeatherNext browser sign-in" {
		t.Fatalf("credentials are not usable: %#v", status)
	}
	if _, err := auth.httpClient(context.Background()); err != nil {
		t.Fatalf("saved OAuth credentials cannot initialize Google API client: %v", err)
	}
}

func TestNativeClientPersistsSettingsAndLocationLifecycle(t *testing.T) {
	home := t.TempDir()
	client, err := StartNative(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	call := func(method string, params map[string]any) map[string]any {
		t.Helper()
		data, err := client.Call(context.Background(), method, params, nil)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("decode %s: %v", method, err)
		}
		return result
	}
	call("settings.update", map[string]any{"settings": map[string]any{"target_project": "sample-project-123", "target_dataset": "weathernext_3", "target_table": "weathernext_3_0_0_0p1deg", "default_hours": 24, "max_bytes_billed": int64(20_000_000_000)}, "billing": map[string]any{"usd_per_tib": 6.25}})
	settings := call("settings.get", nil)
	if settings["project_hint"] != nil {
		t.Fatalf("unexpected project hint in unauthenticated test: %#v", settings["project_hint"])
	}
	app := settings["settings"].(map[string]any)
	if app["target_project"] != "sample-project-123" || app["target_table"] != "weathernext_3_0_0_0p1deg" {
		t.Fatalf("settings not persisted: %#v", app)
	}

	location := call("locations.save", map[string]any{"name": "Sample Place", "latitude": 37.6, "longitude": -1.0})["location"].(map[string]any)
	id := location["id"].(string)
	listed := call("locations.list", nil)["locations"].([]any)
	if len(listed) != 1 {
		t.Fatalf("expected one saved place, got %#v", listed)
	}
	deleted := call("locations.delete", map[string]any{"id": id})
	if deleted["deleted"] != true {
		t.Fatalf("place was not deleted: %#v", deleted)
	}
	listed = call("locations.list", nil)["locations"].([]any)
	if len(listed) != 0 {
		t.Fatalf("deleted place reappeared: %#v", listed)
	}
	if client.store.getSetting("locations_empty_by_choice", false) != true {
		t.Fatal("deleting the final location should persist the deliberate empty state")
	}
}

func TestBigQueryQueryUsesNamedParamsAndBilledByteCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bigquery/v2/projects/test-project/queries" {
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["maximumBytesBilled"] != "123456789" {
			t.Errorf("max billed bytes missing: %#v", body["maximumBytesBilled"])
		}
		if body["parameterMode"] != "NAMED" {
			t.Errorf("expected named query parameters, got %#v", body["parameterMode"])
		}
		parameters := body["queryParameters"].([]any)
		if parameters[0].(map[string]any)["name"] != "sample" {
			t.Errorf("unexpected query parameters: %#v", parameters)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobComplete":true,"jobReference":{"jobId":"job-123","location":"US"},"schema":{"fields":[{"name":"answer","type":"INT64"}]},"rows":[{"f":[{"v":"42"}]}],"totalBytesProcessed":"1024","totalBytesBilled":"2048"}`))
	}))
	defer server.Close()
	api := &googleAPI{http: server.Client(), baseURL: server.URL + "/bigquery/v2"}
	var events []map[string]any
	rows, processed, billed, jobID, err := api.query(context.Background(), "test-project", "SELECT @sample", "US", 123456789, []queryParam{{Name: "sample", Type: "INT64", Value: 42}}, func(event map[string]any) { events = append(events, event) }, "test")
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "job-123" || processed != 1024 || billed != 2048 {
		t.Fatalf("unexpected job details: %s %d %d", jobID, processed, billed)
	}
	if len(rows) != 1 || rows[0]["answer"] != "42" {
		t.Fatalf("unexpected query rows: %#v", rows)
	}
	if len(events) != 2 || events[0]["stage"] != "submitted" || events[1]["stage"] != "completed" {
		t.Fatalf("unexpected billing events: %#v", events)
	}
}

func TestBigQueryReadsDatasetRegion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/bigquery/v2/projects/sample-project/datasets/weather_data" {
			t.Fatalf("unexpected dataset request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"location":"EU"}`))
	}))
	defer server.Close()
	api := &googleAPI{http: server.Client(), baseURL: server.URL + "/bigquery/v2"}
	location, err := api.getDatasetLocation(context.Background(), "sample-project", "weather_data")
	if err != nil || location != "EU" {
		t.Fatalf("dataset location=%q err=%v", location, err)
	}
}

func TestForecastSQLUsesEscapedTargetAndGeometryFilter(t *testing.T) {
	query := forecastSQL("project_id.dataset_id.table_id", []int{0, 2})
	if !strings.Contains(query, "`project_id.dataset_id.table_id`") || strings.Count(query, "ST_INTERSECTS") != 2 || !strings.Contains(query, "@latitude_2") {
		t.Fatalf("unexpected forecast SQL: %s", query)
	}
	discovery := discoverySQL("project_id.dataset_id.table_id", "main")
	if !strings.Contains(discovery, "EXTRACT(HOUR FROM t.init_time) IN (0, 6, 12, 18)") {
		t.Fatalf("main cycle filter missing: %s", discovery)
	}
}

func TestNormalizeTimestampMakesPythonCacheKeysPortable(t *testing.T) {
	got := normalizeTimestamp("2026-10-17T18:00:00+00:00")
	if got != "2026-10-17T18:00:00Z" {
		t.Fatalf("normalized timestamp = %q", got)
	}
}

func TestQueryByteCapUsesRemainingLocalBudget(t *testing.T) {
	client, err := StartNative(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	now := utcNow()
	_, err = client.store.db.Exec(`INSERT INTO bq_jobs(project,job_id,operation_id,operation,status,bytes_processed,bytes_billed,submitted_at_utc,completed_at_utc,error) VALUES(?,?,?,?,?,?,?,?,?,NULL)`, "sample-project", "job-existing", nil, "query", "completed", int64(7000), int64(8000), now, now)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := client.queryByteCap(map[string]any{"max_bytes_billed": int64(5000), "max_daily_billed_bytes": int64(10000), "max_monthly_billed_bytes": int64(20000)})
	if err != nil {
		t.Fatal(err)
	}
	if cap != 2000 {
		t.Fatalf("query cap = %d, want 2000 remaining daily bytes", cap)
	}
	_, err = client.store.db.Exec("UPDATE bq_jobs SET bytes_billed=10000 WHERE job_id='job-existing'")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.queryByteCap(map[string]any{"max_daily_billed_bytes": int64(10000)}); err == nil {
		t.Fatal("a reached local daily threshold should block the next query")
	}
}

func TestEmitProtocolEventPreservesTUIEventData(t *testing.T) {
	var received protocol.Message
	emitProtocolEvent(func(message protocol.Message) { received = message }, "billing.job_completed", map[string]any{"bytes_billed": 1024})
	var data map[string]any
	if err := json.Unmarshal(received.Data, &data); err != nil {
		t.Fatal(err)
	}
	if received.Event != "billing.job_completed" || data["bytes_billed"] != float64(1024) {
		t.Fatalf("unexpected event: %#v %#v", received, data)
	}
}
