package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"weathernext/internal/protocol"
)

const sample = `{"retrieved_at_utc":"2026-10-18T08:00:00Z","dataset":"weathernext_3","requested_hours":24,"locations":[{"id":"example-place","name":"Example place","latitude":12.3456,"longitude":45.6789,"products":{"surface":{"init_time_utc":"2026-10-17T18:00:00Z","forecast":[{"time":"2026-10-18T09:00:00Z","temperature_2m_mean":292.35,"temperature_2m_p10":291.65,"temperature_2m_p90":292.95,"total_precipitation_1hr_mean":0.0004,"total_cloud_cover_mean":0.8,"wind_speed_10m_mean":3.1389}]}}}]}`

func TestNavigationAndResize(t *testing.T) {
	m := New([]byte(sample), nil)
	m.onboarding = false
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m.Update(tea.KeyPressMsg(tea.Key{Code: '2'}))
	if m.tab != TabDaily {
		t.Fatalf("tab = %v, want daily", m.tab)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: '1'}))
	m.Update(tea.KeyPressMsg(tea.Key{Code: 'j'}))
	if m.hour != 0 {
		t.Fatalf("hour = %d, want bounded at 0", m.hour)
	}
	render := m.forecastView()
	if !strings.Contains(render, "Example place") || !strings.Contains(render, "Daily") && m.tab == TabDaily {
		t.Fatalf("unexpected small terminal view: %s", render)
	}
}

func TestViewAndPrintShowPercentileSpreadWithoutConfidenceClaim(t *testing.T) {
	m := New([]byte(sample), nil)
	v := m.hourlyView()
	if !strings.Contains(v, "18.5") || !strings.Contains(v, "19.8") {
		t.Fatalf("spread missing: %s", v)
	}
	if !strings.Contains(v, "not a calibrated confidence score") {
		t.Fatalf("missing spread explanation: %s", v)
	}
	var out strings.Builder
	if err := Print([]byte(sample), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Daily summary") || !strings.Contains(out.String(), "hourly-mean low/high") {
		t.Fatalf("static print output incomplete: %s", out.String())
	}
}

func TestDefaultForecastGroupsHoursAndKeepsRawFieldsOnDemand(t *testing.T) {
	m := New([]byte(sample), nil)
	location := m.data.Locations[0]
	product := location.Products["surface"]
	product.Forecast = []map[string]any{
		{"time": "2026-10-18T12:00:00Z", "temperature_2m_mean": 293.15, "temperature_2m_p10": 292.15, "temperature_2m_p90": 294.15, "total_precipitation_1hr_mean": 0.0004, "total_cloud_cover_mean": 0.8, "wind_speed_10m_mean": 3.0, "u_component_of_wind_10m_mean": 2.0, "v_component_of_wind_10m_mean": -1.0, "dewpoint_temperature_2m_mean": 290.0},
		{"time": "2026-10-18T13:00:00Z", "temperature_2m_mean": 295.15, "temperature_2m_p10": 294.15, "temperature_2m_p90": 296.15, "total_precipitation_1hr_mean": 0.001, "total_cloud_cover_mean": 0.6, "wind_speed_10m_mean": 4.0, "u_component_of_wind_10m_mean": 2.0, "v_component_of_wind_10m_mean": -1.0, "dewpoint_temperature_2m_mean": 291.0},
	}
	location.Products["surface"] = product
	m.data.Locations[0] = location
	m.width, m.height, m.noColor = 120, 40, true
	view := m.summaryView(88)
	for _, want := range []string{"Noon", "(_________)", "/ / / /", "Rain 1.4mm", "Wind", "COMPLETE READABLE FORECAST DATA", "Temperature 2 m"} {
		if !strings.Contains(view, want) {
			t.Fatalf("summary missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, "dewpoint_temperature_2m_mean") || strings.Contains(view, "temperature_2m_p10") {
		t.Fatalf("raw model fields leaked into the default forecast: %s", view)
	}
	rawView := m.detailsView(88)
	if !strings.Contains(rawView, "dewpoint_temperature_2m_mean") {
		t.Fatalf("raw model fields are unavailable on demand: %s", rawView)
	}
}

func TestDailyTabUsesCardsAndReadableCompleteData(t *testing.T) {
	m := New([]byte(sample), nil)
	m.width, m.height = 120, 40
	m.screen = ScreenForecast
	m.tab = TabDaily
	view := m.dailyView(88)
	for _, want := range []string{"Sun 18 Oct", "Avg", "Low/high", "Rain", "Wind", "Cloud", "COMPLETE READABLE DAILY DATA", "Lines 1–"} {
		if !strings.Contains(view, want) {
			t.Fatalf("daily view missing %q: %s", want, view)
		}
	}
	seenTemperature := false
	for i := 0; i < 100; i++ {
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		if strings.Contains(m.dailyView(88), "Temperature 2 m") {
			seenTemperature = true
		}
	}
	if !seenTemperature {
		t.Fatalf("daily readable card did not expose the sanitized model fields: %s", m.dailyView(88))
	}
}

func TestBillingViewEstimatesUSDFromBilledBytes(t *testing.T) {
	m := New([]byte(sample), nil)
	m.spending = json.RawMessage(`{"reported_bytes_billed":1099511627776,"estimated_cost_usd":6.25,"estimate_rate_usd_per_tib":6.25,"daily_bytes_billed":1073741824,"monthly_bytes_billed":1099511627776}`)
	view := m.spendingView()
	for _, want := range []string{"Billed bytes: 1.000 TiB", "Estimated cost: $6.250000 USD", "$6.25/TiB configured rate", "Today: 1.000 GiB"} {
		if !strings.Contains(view, want) {
			t.Fatalf("billing view missing %q: %s", want, view)
		}
	}
	m.spending = json.RawMessage(`{"reported_bytes_billed":1099511627776,"estimated_cost_usd":null,"estimate_rate_usd_per_tib":null}`)
	if view := m.spendingView(); !strings.Contains(view, "Estimated cost: $6.250000 USD") || !strings.Contains(view, "default on-demand rate") || !strings.Contains(view, "Gross on-demand estimate") {
		t.Fatalf("billing view should estimate from bytes when no custom rate is set: %s", view)
	}
}

func TestRawDataScrollsFieldsAndChangesHourHorizontally(t *testing.T) {
	m := New([]byte(sample), nil)
	m.width, m.height = 80, 24
	m.screen = ScreenForecast
	location := m.data.Locations[0]
	product := location.Products["surface"]
	for i := 0; i < 30; i++ {
		product.Forecast[0][fmt.Sprintf("field_%02d", i)] = float64(i)
	}
	second := make(map[string]any, len(product.Forecast[0]))
	for key, value := range product.Forecast[0] {
		second[key] = value
	}
	second["time"] = "2026-10-18T10:00:00Z"
	product.Forecast = append(product.Forecast, second)
	location.Products["surface"] = product
	m.data.Locations[0] = location
	m.tab = TabDetails
	firstPage := m.detailsView(76)
	if !strings.Contains(firstPage, "RAW JSON") || !strings.Contains(firstPage, `"field_00"`) || strings.Contains(firstPage, `"field_29"`) {
		t.Fatalf("raw view did not page the model fields: %s", firstPage)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	secondPage := m.detailsView(76)
	if m.hour != 0 || m.detailScroll != 1 || secondPage == firstPage {
		t.Fatalf("Down did not scroll raw fields independently of time (hour=%d, scroll=%d)", m.hour, m.detailScroll)
	}
	seenLastCustomField := false
	for i := 0; i < 100; i++ {
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
		if strings.Contains(m.detailsView(76), `"field_29"`) {
			seenLastCustomField = true
		}
	}
	lastPage := m.detailsView(76)
	if !seenLastCustomField || !strings.Contains(lastPage, "JSON lines ") {
		t.Fatalf("raw field list could not be scrolled through to the end: %s", lastPage)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
	if m.hour != 1 || m.detailScroll != 0 {
		t.Fatalf("Right did not select the next hour and reset field scroll (hour=%d, scroll=%d)", m.hour, m.detailScroll)
	}
}

func TestEveryScreenWrapsToTerminalWidth(t *testing.T) {
	for _, screen := range []Screen{ScreenForecast, ScreenSearch, ScreenSettings, ScreenLogin, ScreenSpending, ScreenHelp} {
		m := New([]byte(sample), nil)
		m.screen = screen
		m.width = 32
		m.noColor = true
		m.status = strings.Repeat("long status text ", 8)
		m.search = strings.Repeat("searchterm", 8)
		m.loginMessage = "https://accounts.google.com/o/oauth2/device/code/" + strings.Repeat("longtoken", 8)
		m.loginFlow = true
		m.login = json.RawMessage(`{"instructions":"` + strings.Repeat("long login detail ", 8) + `"}`)
		m.spending = json.RawMessage(`{"summary":"` + strings.Repeat("long billing detail ", 8) + `"}`)
		view := m.View().Content
		for lineNo, line := range strings.Split(view, "\n") {
			if width := lipgloss.Width(line); width > m.width {
				t.Errorf("screen %d line %d has width %d, terminal width is %d: %q", screen, lineNo+1, width, m.width, line)
			}
		}
	}
}

func TestWorkspaceFillsScreenWithFramedPanes(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenForecast
	m.onboarding = false
	m.width, m.height, m.noColor = 120, 40, true
	view := m.View()
	if !view.AltScreen {
		t.Fatal("full-screen alternate buffer is not enabled")
	}
	lines := strings.Split(view.Content, "\n")
	if len(lines) != m.height {
		t.Fatalf("rendered height = %d, want terminal height %d (header=%d nav=%d workspace=%d footer=%d left=%d right=%d)", len(lines), m.height,
			lipgloss.Height(m.panel("header", m.headerSubtitle(), m.width, 4, true)),
			lipgloss.Height(m.panel("nav", m.navigation(), m.width, 4, false)),
			lipgloss.Height(m.workspace(m.width, 27)),
			lipgloss.Height(m.panel("footer", "status\nkeys", m.width, 5, false)),
			lipgloss.Height(m.panel("left", m.sidebar(), 27, 27, true)),
			lipgloss.Height(m.panel("right", m.summaryView(88), 92, 27, true)))
	}
	for i, line := range lines {
		if width := lipgloss.Width(line); width != m.width {
			t.Fatalf("rendered line %d width = %d, want terminal width %d: %q", i+1, width, m.width, line)
		}
	}
	if !strings.Contains(view.Content, "╭") || !strings.Contains(view.Content, "WEATHER AT A GLANCE") || !strings.Contains(view.Content, "LOCATIONS") {
		t.Fatalf("workspace is missing framed dashboard panes: %s", view.Content)
	}
}

func TestLoginScreenExplainsWeatherNextTargetWithoutDumpingJSON(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenLogin
	m.onboarding = true
	m.width, m.height = 120, 40
	m.login = json.RawMessage(`{"authenticated":true,"connected":false,"target_project":"test-weather-project","table":"test-weather-project.test_dataset.forecast_table","message":"Google access needs to be checked."}`)
	m.loginMessage = "Google access needs to be checked."
	view := m.loginView()
	for _, want := range []string{"test-weather-project", "forecast_table", "Google sign-in is active", "Access:  not verified"} {
		if !strings.Contains(view, want) {
			t.Fatalf("login view missing %q: %s", want, view)
		}
	}
	for _, forbidden := range []string{`"authenticated"`, `"account_hint"`, `"connected"`} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("login view exposed raw JSON key %q: %s", forbidden, view)
		}
	}
	m.login = json.RawMessage(`{"status":"needs_input"}`)
	m.settingsValues = map[string]any{
		"target_project": "test-weather-project",
		"target_dataset": "test_dataset",
		"target_table":   "forecast_table",
	}
	view = m.loginView()
	if !strings.Contains(view, "Project: test-weather-project") || !strings.Contains(view, "Dataset: test_dataset") || !strings.Contains(view, "Table:   forecast_table") {
		t.Fatalf("login flow stopped showing the configured WeatherNext target: %s", view)
	}
}

func TestStartupChecksGoogleBeforeShowingSavedForecast(t *testing.T) {
	f := &fakeCaller{called: make(chan string, 1), cancel: make(chan struct{})}
	m := New([]byte(sample), f)
	if m.screen != ScreenLogin || !m.onboarding {
		t.Fatalf("startup screen = %v, onboarding=%v", m.screen, m.onboarding)
	}
	if strings.Contains(m.View().Content, "12.34560, 45.67890") {
		t.Fatalf("saved forecast was visible before Google access was checked:\n%s", m.View().Content)
	}
	if cmd := m.Init(); cmd == nil || m.busy != "settings.get" {
		t.Fatal("startup did not load target configuration before checking Google access")
	}
	_, cmd := m.Update(backendResult{method: "settings.get", data: json.RawMessage(`{"settings":{}}`)})
	if cmd == nil || m.busy != "auth.status" {
		t.Fatal("startup did not check Google sign-in after loading local settings")
	}
	_, cmd = m.Update(backendResult{method: "auth.status", data: json.RawMessage(`{"authenticated":false,"login_available":true,"message":"Sign in first."}`)})
	if cmd == nil || m.screen != ScreenLogin || m.busy != "auth.start" || !m.loginAutoStarted {
		t.Fatal("missing credentials did not start the login onboarding flow")
	}
	_, cmd = m.Update(backendResult{method: "auth.start", data: json.RawMessage(`{"status":"complete"}`)})
	if cmd == nil || m.busy != "auth.check" {
		t.Fatal("completed login did not proceed to target access check")
	}
	_, cmd = m.Update(backendResult{method: "auth.check", data: json.RawMessage(`{"authenticated":true,"connected":false,"requires_config":true,"message":"Configure the target."}`)})
	if cmd != nil || m.screen != ScreenSettings || !m.onboarding {
		t.Fatal("missing target config did not route onboarding to Settings")
	}
	_, cmd = m.Update(backendResult{method: "settings.update", data: json.RawMessage(`{"settings":{"target_project":"weather-project","target_dataset":"weather_data","target_table":"surface_forecast"}}`)})
	if cmd == nil || m.busy != "auth.check" {
		t.Fatal("completed target config did not trigger a table access check")
	}
	_, cmd = m.Update(backendResult{method: "auth.check", data: json.RawMessage(`{"connected":true,"message":"Google connection verified."}`)})
	if cmd == nil || m.screen != ScreenLogin || !m.onboarding || m.busy != "maps.setup" || !m.mapsSettingUp {
		t.Fatal("Google Maps was not configured after WeatherNext access succeeded")
	}
	_, cmd = m.Update(backendResult{method: "maps.setup", data: json.RawMessage(`{"configured":true,"message":"Google Maps is ready."}`)})
	if cmd == nil || m.screen != ScreenForecast || m.onboarding || m.busy != "forecast.open" || !m.mapsConfigured {
		t.Fatal("forecast did not open after automatic Google Maps setup")
	}
}

func TestGoogleSignInOpensBrowserAutomatically(t *testing.T) {
	m := NewWithPath(nil, nil, "")
	var opened string
	m.openBrowser = func(value string) error {
		opened = value
		return nil
	}
	_, cmd := m.Update(backendResult{method: "auth.start", data: json.RawMessage(`{"status":"started","request_id":"login-1","verification_url":"https://accounts.google.com/o/oauth2/v2/auth?client_id=public","message":"Sign in with Google."}`)})
	if cmd == nil || opened != "https://accounts.google.com/o/oauth2/v2/auth?client_id=public" {
		t.Fatalf("browser sign-in did not open automatically: opened=%q cmd=%v", opened, cmd != nil)
	}
	if !m.loginFlow || !m.loginBrowserOpened {
		t.Fatalf("login flow state was not retained: flow=%v opened=%v", m.loginFlow, m.loginBrowserOpened)
	}
}

func TestPasteOnlyFillsUserEditableFields(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenLogin
	m.loginFlow = true
	m.Update(tea.PasteMsg{Content: "  ABCD-EFGH\r\n"})
	if strings.Contains(m.status, "pasted") {
		t.Fatalf("browser sign-in should not request a code paste: %q", m.status)
	}

	m.screen = ScreenSearch
	m.Update(tea.PasteMsg{Content: "Example City, Testland\n"})
	if m.search != "Example City, Testland" {
		t.Fatalf("pasted search = %q", m.search)
	}

	m.screen = ScreenSettings
	m.settingIndex = 0
	m.Update(tea.PasteMsg{Content: "sample-project\n"})
	if m.settingsEdit != "sample-project" {
		t.Fatalf("pasted project ID = %q", m.settingsEdit)
	}
}

func TestSettingsSelectionUsesArrowKeysInsteadOfTab(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenSettings
	if m.settingIndex != 0 {
		t.Fatalf("initial settings selection = %d, want 0", m.settingIndex)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if m.settingIndex != 0 {
		t.Fatalf("Tab changed settings selection to %d", m.settingIndex)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if m.settingIndex != 1 {
		t.Fatalf("Down selected settings row %d, want 1", m.settingIndex)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if m.settingIndex != 0 {
		t.Fatalf("Up selected settings row %d, want 0", m.settingIndex)
	}
	for range len(settingRows()) + 2 {
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	}
	if m.settingIndex != 0 {
		t.Fatalf("Up moved before first settings row: %d", m.settingIndex)
	}
	for range len(settingRows()) + 2 {
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	}
	if m.settingIndex != len(settingRows())-1 {
		t.Fatalf("Down selected row %d, want last settings row %d", m.settingIndex, len(settingRows())-1)
	}
	if !strings.Contains(m.settingsView(), "↑/↓ selects") {
		t.Fatal("settings help text does not explain arrow navigation")
	}
}

func TestSidebarDRemovesSelectedSavedLocation(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenForecast
	m.focusSidebar = true
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'd'}))
	if cmd == nil {
		t.Fatal("d in the focused locations sidebar did not request location removal")
	}
	if result, ok := cmd().(backendResult); !ok || result.method != "locations.delete" {
		t.Fatal("sidebar d key did not target locations.delete")
	}
	if len(m.data.Locations) != 1 {
		t.Fatal("location disappeared before the worker confirmed deletion")
	}
	m.Update(backendResult{method: "locations.delete", data: json.RawMessage(`{"deleted":true,"id":"example-place"}`)})
	if len(m.data.Locations) != 0 {
		t.Fatalf("confirmed deletion left %d saved locations", len(m.data.Locations))
	}
	if !strings.Contains(m.status, "removed") {
		t.Fatalf("deletion status was not reported: %q", m.status)
	}
}

func TestRefreshRRemainsRefreshWhenLocationsSidebarFocused(t *testing.T) {
	m := New([]byte(sample), nil)
	m.screen = ScreenForecast
	m.focusSidebar = true
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'r'}))
	if cmd == nil {
		t.Fatal("r should refresh data even when the locations sidebar is focused")
	}
	if result, ok := cmd().(backendResult); !ok || result.method != "forecast.fetch" {
		t.Fatal("r in the locations sidebar did not trigger a forecast refresh")
	}
	if !strings.Contains(m.keyHints(100), "r refresh") || !strings.Contains(m.keyHints(100), "d delete") {
		t.Fatalf("key hints do not distinguish refresh from deletion: %s", m.keyHints(100))
	}
}

type fakeCaller struct {
	called chan string
	cancel chan struct{}
	fail   bool
}

func (f *fakeCaller) Call(ctx context.Context, method string, _ map[string]any, _ func(protocol.Message)) (json.RawMessage, error) {
	f.called <- method
	if f.fail {
		return nil, errors.New("simulated worker failure")
	}
	<-ctx.Done()
	close(f.cancel)
	return nil, ctx.Err()
}

func TestWorkerErrorAndCancellationRemainResponsive(t *testing.T) {
	f := &fakeCaller{called: make(chan string, 1), cancel: make(chan struct{})}
	m := New([]byte(sample), f)
	m.screen = ScreenForecast
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'r'}))
	if cmd == nil {
		t.Fatal("refresh did not create command")
	}
	done := make(chan tea.Msg, 1)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatal("refresh did not batch worker and event commands")
	}
	for _, subcmd := range batch {
		go func(run tea.Cmd) {
			if msg := run(); msg != nil {
				done <- msg
			}
		}(subcmd)
	}
	select {
	case <-f.called:
	case <-time.After(time.Second):
		t.Fatal("backend call did not start")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	select {
	case <-f.cancel:
	case <-time.After(time.Second):
		t.Fatal("Escape did not cancel backend context")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled backend command did not return")
	}

	f2 := &fakeCaller{called: make(chan string, 1), cancel: make(chan struct{}), fail: true}
	m2 := New([]byte(sample), f2)
	m2.screen = ScreenForecast
	_, cmd = m2.Update(tea.KeyPressMsg(tea.Key{Code: 'r'}))
	batch, ok = cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatal("failed refresh did not create worker command")
	}
	msg := batch[0]().(backendResult)
	if msg.err == nil || msg.err.Error() != "simulated worker failure" {
		t.Fatalf("error = %v", msg.err)
	}
}

func TestPrintActualSavedForecastUsesDisplayUnits(t *testing.T) {
	path := filepath.Clean("../../output/forecast.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("real locally saved forecast unavailable: %v", err)
	}
	var out strings.Builder
	if err := Print(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WeatherNext 3") || !strings.Contains(out.String(), "Hourly forecast") {
		t.Fatalf("unexpected output: %s", out.String()[:min(400, out.Len())])
	}
	if strings.Contains(out.String(), "NaN") {
		t.Fatal("summary should omit null model fields instead of printing NaN")
	}
}
