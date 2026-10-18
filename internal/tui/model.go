package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	keypkg "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"weathernext/internal/protocol"
)

type Caller interface {
	Call(context.Context, string, map[string]any, func(protocol.Message)) (json.RawMessage, error)
}

type Data struct {
	Retrieved string     `json:"retrieved_at_utc"`
	Dataset   string     `json:"dataset"`
	Cycle     string     `json:"cycle"`
	Hours     int        `json:"requested_hours"`
	Units     any        `json:"units"`
	Locations []Location `json:"locations"`
}
type Location struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Latitude    float64            `json:"latitude"`
	Longitude   float64            `json:"longitude"`
	PlaceID     string             `json:"place_id,omitempty"`
	Attribution string             `json:"attribution,omitempty"`
	Products    map[string]Product `json:"products"`
}
type Product struct {
	InitTime string           `json:"init_time_utc"`
	Forecast []map[string]any `json:"forecast"`
}

type Screen int

const (
	ScreenForecast Screen = iota
	ScreenSearch
	ScreenSettings
	ScreenLogin
	ScreenSpending
	ScreenHelp
)

type Tab int

const (
	TabHourly Tab = iota
	TabDaily
	TabDetails
)

var navigationKeys = struct{ Up, Down keypkg.Binding }{
	Up:   keypkg.NewBinding(keypkg.WithKeys("up", "k"), keypkg.WithHelp("↑/k", "move up")),
	Down: keypkg.NewBinding(keypkg.WithKeys("down", "j"), keypkg.WithHelp("↓/j", "move down")),
}

type Model struct {
	data              Data
	caller            Caller
	forecastPath      string
	screen            Screen
	tab               Tab
	selected          int
	hour              int
	detailScroll      int
	readableScroll    int
	spendingScroll    int
	width, height     int
	search            string
	candidates        []Location
	candidate         int
	loginCode         string
	loginFlow         bool
	loginAutoStarted  bool
	onboarding        bool
	authChecking      bool
	mapsSettingUp     bool
	mapsConfigured    bool
	mapsSetupMessage  string
	loginMessage      string
	loginURL          string
	loginRequestID    string
	targetProject     string
	targetTable       string
	targetProjectHint string
	mapsKey           string
	focusSidebar      bool
	status            string
	loading           bool
	busy              string
	spending          json.RawMessage
	jobs              json.RawMessage
	settings          json.RawMessage
	settingsValues    map[string]any
	settingIndex      int
	settingsEdit      string
	login             json.RawMessage
	useASCII          bool
	noColor           bool
	requestCancel     context.CancelFunc
	eventCtx          context.Context
	eventCh           chan backendEvent
}

func New(raw []byte, caller Caller) Model {
	return NewWithPath(raw, caller, "output/forecast.json")
}

func NewWithPath(raw []byte, caller Caller, path string) Model {
	m := Model{caller: caller, forecastPath: path, width: 80, height: 24, noColor: os.Getenv("NO_COLOR") != "",
		screen: ScreenLogin, onboarding: true}
	m.useASCII = !supportsUnicode()
	if strings.EqualFold(os.Getenv("WEATHERNEXT_ASCII"), "1") {
		m.useASCII = true
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &m.data); err != nil {
			m.status = "Could not read forecast: " + err.Error()
		}
	}
	return m
}

func (m *Model) Init() tea.Cmd {
	if m.caller != nil {
		return m.run("settings.get", nil)
	}
	m.authChecking = false
	m.loginMessage = "The Python worker is unavailable. Open the app from the WeatherNext project after setup."
	return nil
}

type backendResult struct {
	method string
	data   json.RawMessage
	err    error
}
type backendEvent struct {
	method string
	msg    protocol.Message
}
type loginPollMsg struct{}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.PasteMsg:
		pasted := cleanPastedText(msg.Content)
		if pasted == "" {
			return m, nil
		}
		switch m.screen {
		case ScreenLogin:
			if m.loginFlow {
				m.loginCode += pasted
				m.status = "Login code pasted · press Enter to submit"
			}
		case ScreenSearch:
			m.search += pasted
			m.candidates = nil
		case ScreenSettings:
			if m.settingIndex == len(settingRows()) {
				m.mapsKey += pasted
			} else {
				m.settingsEdit += pasted
			}
		}
	case backendEvent:
		if msg.msg.Event == "billing.job_completed" {
			var b struct {
				BytesBilled int64  `json:"bytes_billed"`
				JobID       string `json:"job_id"`
				Operation   string `json:"operation"`
			}
			_ = json.Unmarshal(msg.msg.Data, &b)
			m.status = fmt.Sprintf("%s billed %s · job %s", b.Operation, formatBytes(b.BytesBilled), short(b.JobID))
		} else {
			var e map[string]any
			_ = json.Unmarshal(msg.msg.Data, &e)
			if p, ok := e["message"].(string); ok && p != "" {
				m.status = p
			}
		}
		return m, m.waitEvent()
	case loginPollMsg:
		if m.screen == ScreenLogin && m.loginFlow && !m.loading {
			return m, m.run("auth.respond", map[string]any{"request_id": m.loginRequestID})
		}
	case backendResult:
		m.loading = false
		m.busy = ""
		if msg.err != nil {
			if msg.method == "auth.check" {
				m.authChecking = false
				m.onboarding = true
				m.screen = ScreenLogin
				m.loginMessage = "Could not verify Google access: " + msg.err.Error()
			}
			m.status = msg.err.Error()
			return m, nil
		}
		switch msg.method {
		case "auth.check":
			var check struct {
				Connected      bool   `json:"connected"`
				RequiresLogin  bool   `json:"requires_login"`
				RequiresConfig bool   `json:"requires_config"`
				LoginAvailable bool   `json:"login_available"`
				TargetProject  string `json:"target_project"`
				Table          string `json:"table"`
				Message        string `json:"message"`
			}
			_ = json.Unmarshal(msg.data, &check)
			if check.TargetProject != "" {
				m.targetProject = check.TargetProject
			}
			if check.Table != "" {
				m.targetTable = check.Table
			}
			m.authChecking = false
			m.login = msg.data
			if check.Connected {
				m.mapsSettingUp = true
				m.loginMessage = "WeatherNext access verified. Setting up Google Maps automatically…"
				return m, m.run("maps.setup", nil)
			}
			m.loginMessage = check.Message
			if check.RequiresConfig {
				m.screen = ScreenSettings
				return m, nil
			}
			if check.RequiresLogin || m.onboarding {
				m.screen = ScreenLogin
			} else {
				m.screen = ScreenSettings
			}
			if check.RequiresLogin && check.LoginAvailable && !m.loginAutoStarted {
				m.loginAutoStarted = true
				return m, m.run("auth.start", nil)
			}
		case "maps.setup":
			var setup struct {
				Configured bool   `json:"configured"`
				Message    string `json:"message"`
			}
			_ = json.Unmarshal(msg.data, &setup)
			m.mapsSettingUp = false
			m.mapsConfigured = setup.Configured
			if setup.Configured {
				m.mapsSetupMessage = ""
				m.status = "Google Maps is ready · key restricted to Geocoding API"
			} else {
				m.mapsSetupMessage = setup.Message
				m.status = setup.Message
			}
			if m.onboarding {
				m.onboarding = false
				m.loginFlow = false
				m.screen = ScreenForecast
				return m, m.run("forecast.open", map[string]any{"path": m.forecastPath})
			}
		case "forecast.fetch", "forecast.open":
			var result struct {
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal(msg.data, &result) == nil && len(result.Output) > 0 {
				var loaded Data
				if err := json.Unmarshal(result.Output, &loaded); err != nil {
					m.status = "Forecast response could not be parsed: " + err.Error()
				} else {
					m.acceptForecast(loaded, msg.method == "forecast.fetch")
					m.status = "Forecast ready · " + m.data.Retrieved
				}
			} else {
				var loaded Data
				if err := json.Unmarshal(msg.data, &loaded); err != nil {
					m.status = "Forecast response is empty or invalid"
				} else {
					m.acceptForecast(loaded, msg.method == "forecast.fetch")
					m.status = "Forecast ready · " + m.data.Retrieved
				}
			}
			if msg.method == "forecast.open" {
				return m, m.run("locations.list", nil)
			}
		case "spending.summary":
			m.spending = msg.data
			return m, m.run("spending.jobs", nil)
		case "spending.jobs":
			m.jobs = msg.data
		case "settings.get":
			m.settings = msg.data
			m.decodeSettings(msg.data)
			if m.onboarding {
				return m, m.run("auth.status", nil)
			}
		case "settings.update":
			m.settings = msg.data
			m.decodeSettings(msg.data)
			m.status = "Settings saved locally"
			if isTargetField(m.settingField()) && m.hasTargetConfig() {
				m.authChecking = true
				if m.onboarding {
					m.screen = ScreenLogin
				}
				m.loginMessage = "Checking the updated target table metadata…"
				return m, m.run("auth.check", m.targetParams())
			}
			if m.onboarding {
				if m.hasTargetConfig() {
					m.authChecking = true
					m.screen = ScreenLogin
					m.loginMessage = "Target saved. Checking table access before opening forecasts…"
					return m, m.run("auth.check", m.targetParams())
				}
				m.loginMessage = "Saved. Complete the project, dataset, and table fields to verify access."
			}
		case "auth.status":
			m.login = msg.data
			var state struct {
				Authenticated  bool   `json:"authenticated"`
				LoginAvailable bool   `json:"login_available"`
				ProjectHint    string `json:"project_hint"`
				Message        string `json:"message"`
				Login          struct {
					Status          string `json:"status"`
					RequestID       string `json:"request_id"`
					VerificationURL string `json:"verification_url"`
					Instructions    string `json:"instructions"`
				} `json:"login"`
			}
			_ = json.Unmarshal(msg.data, &state)
			if state.ProjectHint != "" {
				m.targetProjectHint = state.ProjectHint
			}
			if state.Login.RequestID != "" {
				m.loginRequestID = state.Login.RequestID
			}
			m.loginMessage = strings.TrimSpace(strings.Join([]string{state.Message, state.Login.Instructions, state.Login.VerificationURL}, "\n"))
			m.loginURL = state.Login.VerificationURL
			if state.Authenticated || state.Login.Status == "complete" {
				m.loginFlow = false
				if m.onboarding {
					if !m.hasTargetConfig() {
						m.screen = ScreenSettings
						m.loginMessage = "Google credentials are ready. Configure the WeatherNext target table below."
						return m, nil
					}
					m.authChecking = true
					return m, m.run("auth.check", m.targetParams())
				}
				m.loginMessage = "Application Default Credentials are ready."
			} else if m.onboarding && state.LoginAvailable {
				m.loginMessage = "No usable Google credentials were found. Starting Google sign-in…"
				if !m.loginAutoStarted {
					m.loginAutoStarted = true
					return m, m.run("auth.start", nil)
				}
			} else if state.Login.Status == "started" || state.Login.Status == "needs_input" {
				m.loginFlow = true
				return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return loginPollMsg{} })
			} else if state.Login.Status == "failed" || state.Login.Status == "cancelled" {
				m.loginFlow = false
			}
		case "auth.start", "auth.respond":
			m.login = msg.data
			var flow struct {
				Status          string `json:"status"`
				RequestID       string `json:"request_id"`
				VerificationURL string `json:"verification_url"`
				Instructions    string `json:"instructions"`
				Message         string `json:"message"`
			}
			_ = json.Unmarshal(msg.data, &flow)
			if flow.RequestID != "" {
				m.loginRequestID = flow.RequestID
			}
			m.loginFlow = flow.Status == "needs_input" || flow.Status == "started"
			m.loginMessage = strings.TrimSpace(strings.Join([]string{flow.Message, flow.Instructions, flow.VerificationURL}, "\n"))
			m.loginURL = flow.VerificationURL
			if flow.Status == "complete" {
				m.loginFlow = false
				m.loginMessage = "Google sign-in finished. Verifying WeatherNext access…"
				m.authChecking = true
				return m, m.run("auth.check", m.targetParams())
			}
			if m.loginFlow {
				return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return loginPollMsg{} })
			}
		case "locations.list":
			var result struct {
				Locations []Location `json:"locations"`
			}
			if err := json.Unmarshal(msg.data, &result); err == nil {
				for i := range result.Locations {
					for _, cached := range m.data.Locations {
						if math.Abs(cached.Latitude-result.Locations[i].Latitude) < 0.00001 && math.Abs(cached.Longitude-result.Locations[i].Longitude) < 0.00001 {
							result.Locations[i].Products = cached.Products
							if result.Locations[i].Name == "" {
								result.Locations[i].Name = cached.Name
							}
							break
						}
					}
				}
				selectedID := ""
				if current := m.selectedLocation(); current != nil {
					selectedID = current.ID
				}
				m.data.Locations = result.Locations
				for i, location := range result.Locations {
					if location.ID == selectedID {
						m.selected = i
						break
					}
				}
				if m.selected >= len(m.data.Locations) {
					m.selected = max(0, len(m.data.Locations)-1)
				}
			}
		case "locations.search":
			var result struct {
				Candidates []Location `json:"candidates"`
				Status     string     `json:"status"`
				Message    string     `json:"message"`
			}
			if err := json.Unmarshal(msg.data, &result); err == nil {
				m.candidates = result.Candidates
				m.candidate = 0
				m.status = result.Message
				if result.Status != "ok" && result.Status != "ambiguous" && result.Message == "" {
					m.status = "Search: " + result.Status
				}
			}
		case "locations.save":
			var result struct {
				Location Location `json:"location"`
			}
			if err := json.Unmarshal(msg.data, &result); err == nil && result.Location.ID != "" {
				m.data.Locations = append(m.data.Locations, result.Location)
				m.selected = len(m.data.Locations) - 1
				m.status = "Saved " + result.Location.Name
				m.screen = ScreenForecast
				return m, m.run("locations.list", nil)
			}
		case "locations.delete":
			var result struct {
				Deleted bool   `json:"deleted"`
				ID      string `json:"id"`
			}
			if json.Unmarshal(msg.data, &result) == nil && result.Deleted {
				for i, location := range m.data.Locations {
					if location.ID == result.ID {
						m.data.Locations = append(m.data.Locations[:i], m.data.Locations[i+1:]...)
						break
					}
				}
				m.selected = clamp(m.selected, 0, max(0, len(m.data.Locations)-1))
				m.hour, m.readableScroll, m.detailScroll = 0, 0, 0
				m.status = "Saved location removed"
			}
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if keypkg.Matches(msg, navigationKeys.Up) {
			key = "up"
		} else if keypkg.Matches(msg, navigationKeys.Down) {
			key = "down"
		}
		if m.screen == ScreenSearch {
			switch key {
			case "esc":
				if m.loading && m.requestCancel != nil {
					m.requestCancel()
					m.status = "Cancellation requested…"
					return m, nil
				}
				m.screen = ScreenForecast
				m.search = ""
				m.candidates = nil
			case "enter":
				if m.loading {
					return m, nil
				}
				if len(m.candidates) > 0 {
					c := m.candidates[clamp(m.candidate, 0, len(m.candidates)-1)]
					return m, m.run("locations.save", map[string]any{"name": c.Name, "latitude": c.Latitude, "longitude": c.Longitude, "place_id": c.PlaceID, "attribution": c.Attribution})
				}
				if strings.TrimSpace(m.search) != "" {
					params := map[string]any{"query": strings.TrimSpace(m.search)}
					if m.mapsKey != "" {
						params["api_key"] = m.mapsKey
					}
					return m, m.run("locations.search", params)
				}
			case "up", "k":
				if len(m.candidates) > 0 {
					m.candidate = clamp(m.candidate-1, 0, len(m.candidates)-1)
				}
			case "down", "j":
				if len(m.candidates) > 0 {
					m.candidate = clamp(m.candidate+1, 0, len(m.candidates)-1)
				}
			case "backspace":
				if len(m.search) > 0 {
					m.search = m.search[:len(m.search)-1]
					m.candidates = nil
				}
			default:
				if text := msg.Key().Text; text != "" {
					m.search += text
					m.candidates = nil
				}
			}
			return m, nil
		}
		if m.screen == ScreenLogin {
			if key == "esc" {
				if m.loading && m.requestCancel != nil {
					m.requestCancel()
					m.status = "Cancellation requested…"
					return m, nil
				}
				if m.loginFlow {
					return m, m.run("auth.respond", map[string]any{"request_id": m.loginRequestID, "action": "cancel"})
				}
				m.screen = ScreenForecast
				return m, nil
			}
			if key == "a" && !m.loading {
				return m, m.run("auth.start", nil)
			}
			if key == "o" && m.loginURL != "" {
				if err := openVerificationURL(m.loginURL); err != nil {
					m.status = err.Error()
				} else {
					m.status = "Opened Google's login page in your browser."
				}
				return m, nil
			}
			if key == "enter" && m.loginFlow && m.loginCode != "" {
				code := m.loginCode
				m.loginCode = ""
				return m, m.run("auth.respond", map[string]any{"request_id": m.loginRequestID, "code": code})
			}
			if key == "backspace" && len(m.loginCode) > 0 {
				m.loginCode = m.loginCode[:len(m.loginCode)-1]
				return m, nil
			}
			if text := msg.Key().Text; text != "" && m.loginFlow {
				m.loginCode += text
				return m, nil
			}
		}
		if m.screen == ScreenSettings {
			if key == "esc" {
				if m.loading && m.requestCancel != nil {
					m.requestCancel()
					m.status = "Cancellation requested…"
					return m, nil
				}
				if m.onboarding {
					m.screen = ScreenLogin
				} else {
					m.screen = ScreenForecast
				}
				return m, nil
			}
			if key == "up" || key == "down" {
				delta := -1
				if key == "down" {
					delta = 1
				}
				m.settingIndex = clamp(m.settingIndex+delta, 0, len(settingRows()))
				m.settingsEdit = ""
				return m, nil
			}
			if key == "ctrl+g" && !m.loading {
				m.status = "Configuring the Geocoding API in your Google Cloud project…"
				return m, m.run("maps.setup", nil)
			}
			if key == "backspace" && len(m.settingsEdit) > 0 {
				m.settingsEdit = m.settingsEdit[:len(m.settingsEdit)-1]
				return m, nil
			}
			if m.settingField() == "units" && (key == "c" || key == "f") {
				m.settingsValues["units"] = strings.ToUpper(key)
				return m, m.saveSettings()
			}
			if key == "ctrl+t" && !m.loading {
				if !m.hasTargetConfig() {
					m.status = "Set the project ID, dataset, and table before checking access."
					return m, nil
				}
				m.authChecking = true
				m.loginMessage = "Checking table metadata only; no forecast query will run…"
				return m, m.run("auth.check", m.targetParams())
			}
			if key == "ctrl+p" && m.settingField() == "target_project" && m.targetProjectHint != "" {
				m.settingsEdit = m.targetProjectHint
				return m, nil
			}
			if key == "enter" {
				if m.settingIndex == len(settingRows()) {
					m.status = "Maps key is held in memory for this session only"
					return m, nil
				}
				return m, m.saveSettings()
			}
			if text := msg.Key().Text; text != "" {
				if m.settingIndex == len(settingRows()) {
					m.mapsKey += text
				} else {
					m.settingsEdit += text
				}
				return m, nil
			}
		}
		switch key {
		case "ctrl+c", "q":
			if m.loading && m.requestCancel != nil {
				m.requestCancel()
				m.status = "Cancellation requested…"
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.loading && m.requestCancel != nil {
				m.requestCancel()
				m.status = "Cancellation requested…"
			} else if m.screen != ScreenForecast {
				m.screen = ScreenForecast
			}
		case "up", "k":
			if m.screen == ScreenSpending {
				m.scrollSpending(-1)
			} else if m.screen == ScreenForecast {
				if m.tab == TabDetails && !m.focusSidebar {
					m.scrollDetails(-1)
				} else if !m.focusSidebar && (m.tab == TabHourly || m.tab == TabDaily) {
					m.scrollReadable(-1)
				} else {
					m.move(-1)
				}
			}
		case "down", "j":
			if m.screen == ScreenSpending {
				m.scrollSpending(1)
			} else if m.screen == ScreenForecast {
				if m.tab == TabDetails && !m.focusSidebar {
					m.scrollDetails(1)
				} else if !m.focusSidebar && (m.tab == TabHourly || m.tab == TabDaily) {
					m.scrollReadable(1)
				} else {
					m.move(1)
				}
			}
		case "left", "h":
			if m.screen == ScreenForecast {
				if !m.focusSidebar && (m.tab == TabDetails || m.tab == TabHourly || m.tab == TabDaily) && key == "left" {
					m.move(-1)
				} else {
					m.focusSidebar = true
				}
			} else if m.screen == ScreenSearch && len(m.candidates) > 0 {
				m.candidate = clamp(m.candidate-1, 0, len(m.candidates)-1)
			}
		case "right":
			if m.screen == ScreenForecast {
				if !m.focusSidebar && (m.tab == TabDetails || m.tab == TabHourly || m.tab == TabDaily) {
					m.move(1)
				} else {
					m.focusSidebar = false
				}
			} else if m.screen == ScreenSearch && len(m.candidates) > 0 {
				m.candidate = clamp(m.candidate+1, 0, len(m.candidates)-1)
			}
		case "tab":
			if m.screen == ScreenForecast {
				m.tab = (m.tab + 1) % 3
			}
		case "1":
			m.screen = ScreenForecast
			m.tab = TabHourly
		case "2":
			m.screen = ScreenForecast
			m.tab = TabDaily
		case "3":
			m.screen = ScreenForecast
			m.tab = TabDetails
			m.detailScroll = 0
		case "/":
			m.screen = ScreenSearch
			m.search = ""
		case "d":
			if m.screen == ScreenForecast && m.focusSidebar {
				if m.loading {
					return m, nil
				}
				ids := m.selectedIDs()
				if len(ids) == 0 {
					m.status = "Select a saved location to remove."
					return m, nil
				}
				return m, m.run("locations.delete", map[string]any{"id": ids[0]})
			}
		case "r":
			if !m.loading {
				if len(m.selectedIDs()) == 0 {
					m.status = "No saved location is selected. The cached forecast will be imported when the worker opens it."
					return m, nil
				}
				return m, m.run("forecast.fetch", map[string]any{"location_ids": m.selectedIDs(), "hours": m.forecastHours(), "include_station": false, "refresh": true})
			}
		case "b":
			m.screen = ScreenSpending
			m.spendingScroll = 0
			if !m.loading {
				return m, m.run("spending.summary", nil)
			}
		case "s":
			m.screen = ScreenSettings
			if m.settingsValues == nil {
				m.settingsValues = map[string]any{"default_hours": 24, "units": "C"}
			}
			if !m.loading {
				return m, m.run("settings.get", nil)
			}
		case "l":
			m.screen = ScreenLogin
			if !m.loading {
				return m, m.run("auth.status", nil)
			}
		case "?":
			m.screen = ScreenHelp
		case "enter":
			if m.screen == ScreenLogin && m.loginFlow && m.loginCode != "" {
				code := m.loginCode
				m.loginCode = ""
				return m, m.run("auth.respond", map[string]any{"request_id": m.loginRequestID, "code": code})
			}
			if m.screen == ScreenForecast && m.tab == TabHourly {
				m.tab = TabDetails
			}
		}
	}
	return m, nil
}

func cleanPastedText(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func preserveLocationIdentity(data *Data, saved []Location) {
	for i := range data.Locations {
		for _, previous := range saved {
			if math.Abs(previous.Latitude-data.Locations[i].Latitude) < 0.00001 &&
				math.Abs(previous.Longitude-data.Locations[i].Longitude) < 0.00001 {
				if previous.ID != "" {
					data.Locations[i].ID = previous.ID
				}
				data.Locations[i].PlaceID = previous.PlaceID
				data.Locations[i].Attribution = previous.Attribution
				if data.Locations[i].Name == "" {
					data.Locations[i].Name = previous.Name
				}
				break
			}
		}
	}
}

func (m *Model) acceptForecast(loaded Data, refresh bool) {
	preserveLocationIdentity(&loaded, m.data.Locations)
	if refresh {
		// Fetch returns only the requested place; keep the rest of the saved list
		// and its cached forecasts in the workspace.
		places := append([]Location(nil), m.data.Locations...)
		for _, incoming := range loaded.Locations {
			found := false
			for i, existing := range places {
				if incoming.ID != "" && incoming.ID == existing.ID {
					places[i] = incoming
					found = true
					break
				}
			}
			if !found {
				places = append(places, incoming)
			}
		}
		loaded.Locations = places
	}
	m.data = loaded
	m.selected = clamp(m.selected, 0, max(0, len(m.data.Locations)-1))
	m.hour = clamp(m.hour, 0, max(0, m.hourCount()-1))
	m.readableScroll, m.detailScroll = 0, 0
}

func (m *Model) move(delta int) {
	if m.focusSidebar {
		if len(m.data.Locations) > 0 {
			next := clamp(m.selected+delta, 0, len(m.data.Locations)-1)
			if next != m.selected {
				m.hour, m.readableScroll, m.detailScroll = 0, 0, 0
			}
			m.selected = next
		}
		return
	}
	if m.tab == TabDaily {
		starts := forecastDayStarts(m.currentProduct())
		if len(starts) > 0 {
			selected := 0
			for i, start := range starts {
				if start <= m.hour {
					selected = i
				}
			}
			selected = clamp(selected+delta, 0, len(starts)-1)
			m.hour = starts[selected]
			m.readableScroll = 0
		}
		return
	}
	if m.tab != TabDaily {
		if m.tab == TabHourly {
			groups := forecastPeriods(m.currentProduct())
			if len(groups) > 0 {
				selected := periodForHour(groups, m.hour)
				selected = clamp(selected+delta, 0, len(groups)-1)
				m.hour = groups[selected].startIndex
				m.readableScroll = 0
				return
			}
		}
		m.hour = clamp(m.hour+delta, 0, max(0, m.hourCount()-1))
		if m.tab == TabDetails {
			m.detailScroll = 0
		} else {
			m.readableScroll = 0
		}
	}
}

func forecastDayStarts(p *Product) []int {
	if p == nil {
		return nil
	}
	starts := make([]int, 0)
	lastDate := ""
	for i, row := range p.Forecast {
		ts, err := time.Parse(time.RFC3339, str(row["time"]))
		if err != nil {
			continue
		}
		date := ts.Local().Format("2006-01-02")
		if date != lastDate {
			starts = append(starts, i)
			lastDate = date
		}
	}
	return starts
}

func (m *Model) scrollDetails(delta int) {
	m.detailScroll = clamp(m.detailScroll+delta, 0, m.detailScrollMax())
}

func (m *Model) scrollReadable(delta int) {
	m.readableScroll = clamp(m.readableScroll+delta, 0, m.readableScrollMax())
}

func (m Model) selectedIDs() []string {
	if m.selected < 0 || m.selected >= len(m.data.Locations) {
		return nil
	}
	loc := m.data.Locations[m.selected]
	if loc.ID == "" {
		return nil
	}
	return []string{loc.ID}
}

func (m *Model) run(method string, params map[string]any) tea.Cmd {
	if m.caller == nil {
		return func() tea.Msg {
			return backendResult{method: method, err: fmt.Errorf("Python worker is not available. Open saved forecasts offline or run from the project folder after setup.")}
		}
	}
	m.loading = true
	m.busy = method
	ctx, cancel := context.WithCancel(context.Background())
	m.requestCancel = cancel
	m.eventCtx = ctx
	m.eventCh = make(chan backendEvent, 64)
	callCmd := func() tea.Msg {
		defer cancel()
		data, err := m.caller.Call(ctx, method, params, func(event protocol.Message) {
			select {
			case m.eventCh <- backendEvent{method: method, msg: event}:
			case <-ctx.Done():
			}
		})
		return backendResult{method: method, data: data, err: err}
	}
	return tea.Batch(callCmd, m.waitEvent())
}

func (m *Model) waitEvent() tea.Cmd {
	if m.eventCh == nil || m.eventCtx == nil {
		return nil
	}
	ch, ctx := m.eventCh, m.eventCtx
	return func() tea.Msg {
		select {
		case event := <-ch:
			return event
		case <-ctx.Done():
			return nil
		}
	}
}

func (m Model) frameHeights() (header, navigation, footer, main int) {
	width := max(1, m.width)
	header, footer = 4, 5
	navigation = max(3, lipgloss.Height(lipgloss.Wrap(m.navigation(), max(1, width-4), " "))+3)
	if m.height < 36 {
		header, navigation, footer = 3, 3, 4
	}
	main = m.height - header - navigation - footer
	return
}

func (m Model) View() tea.View {
	width := max(1, m.width)
	headerHeight, navHeight, footerHeight, mainHeight := m.frameHeights()

	if mainHeight < 1 || width < 8 {
		view := tea.NewView(lipgloss.Wrap("Terminal is too small for the WeatherNext workspace; enlarge it to continue.", width, " "))
		view.AltScreen = true
		return view
	}

	header := m.panel("WEATHERNEXT 3  /  WEATHER DESK", m.headerSubtitle(), width, headerHeight, true)
	navigation := m.panel("NAVIGATION", m.navigation(), width, navHeight, false)
	if m.height < 36 {
		title := "WEATHERNEXT 3"
		if m.authChecking {
			title += " · Connecting to Google APIs"
		} else if m.onboarding {
			title += " · Google sign-in"
		}
		if !m.onboarding && m.selected >= 0 && m.selected < len(m.data.Locations) {
			title += " · " + m.data.Locations[m.selected].Name
		}
		header = m.panel(title, "", width, headerHeight, true)
		navigation = m.panel("1 Forecast · 2 Daily · 3 Raw · / Places · b Billing · s Settings", "", width, navHeight, false)
	}
	workspace := m.workspace(width, mainHeight)
	statusTitle, status := "ACTIVITY", m.status
	if m.loading {
		statusTitle = "WORKING  /  " + strings.ToUpper(m.busy)
		status = "Request in progress · Esc asks the worker to cancel"
	}
	if status == "" {
		status = "Forecasts open locally · refresh runs a BigQuery query"
	}
	footer := m.panel(statusTitle, status+"\n"+m.keyHints(width), width, footerHeight, false)
	if m.height < 36 {
		footer = m.panel(status, m.keyHints(width), width, footerHeight, false)
	}
	body := lipgloss.JoinVertical(lipgloss.Left, header, navigation, workspace, footer)
	if m.useASCII {
		body = asciiFallback(body)
	}
	if !m.noColor {
		body = lipgloss.NewStyle().Foreground(lipgloss.Color("#DCE7F5")).Render(body)
	}
	view := tea.NewView(body)
	view.AltScreen = true
	return view
}

func (m Model) forecastView() string {
	return m.View().Content
}

func (m Model) sidebar() string {
	var b strings.Builder
	if len(m.data.Locations) == 0 {
		return "No saved places yet.\n\nPress / to search Google Maps."
	}
	for i, l := range m.data.Locations {
		marker := "  "
		if i == m.selected {
			marker = "● "
			if m.useASCII {
				marker = "> "
			}
		}
		name := l.Name
		if name == "" {
			name = fmt.Sprintf("%.3f, %.3f", l.Latitude, l.Longitude)
		}
		b.WriteString(marker + fitLine(name, 21) + "\n")
		b.WriteString(fmt.Sprintf("   %.4f, %.4f\n", l.Latitude, l.Longitude))
		if p, ok := l.Products["surface"]; ok {
			b.WriteString("   Run " + initTime(&p) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("h then ↑/↓ to switch places\nd to remove selected place\n/ to add another")
	return b.String()
}

func (m Model) workspace(width, height int) string {
	if m.screen != ScreenForecast {
		return m.panel(m.screenTitle(), m.screenContent(), width, height, true)
	}
	if width >= 100 {
		leftWidth := 27
		rightWidth := width - leftWidth - 1
		left := m.panel("LOCATIONS", m.sidebar(), leftWidth, height, m.focusSidebar)
		right := m.panel(m.forecastTitle(), m.forecastContent(rightWidth-4), rightWidth, height, !m.focusSidebar)
		return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	}
	if width < 72 && height >= 8 {
		locationsHeight := min(6, max(4, height/3))
		if height-locationsHeight >= 3 {
			locations := m.panel("LOCATIONS", m.sidebar(), width, locationsHeight, m.focusSidebar)
			forecast := m.panel(m.forecastTitle(), m.forecastContent(width-4), width, height-locationsHeight, !m.focusSidebar)
			return lipgloss.JoinVertical(lipgloss.Left, locations, forecast)
		}
	}
	return m.panel(m.forecastTitle(), m.forecastContent(width-4), width, height, !m.focusSidebar)
}

func (m Model) screenTitle() string {
	switch m.screen {
	case ScreenSearch:
		return "PLACE SEARCH  /  GOOGLE MAPS"
	case ScreenSettings:
		return "PREFERENCES  /  LOCAL SETTINGS"
	case ScreenLogin:
		return "GOOGLE CLOUD  /  SIGN IN"
	case ScreenSpending:
		start, end, total := m.spendingWindow()
		return fmt.Sprintf("USAGE / BILLING  ·  LINES %d–%d OF %d  ·  ↑↓ SCROLL", start+1, end, total)
	case ScreenHelp:
		return "QUICK REFERENCE  /  KEYBOARD"
	default:
		return m.forecastTitle()
	}
}

func (m Model) screenContent() string {
	switch m.screen {
	case ScreenSearch:
		return m.searchView()
	case ScreenSettings:
		return m.settingsView()
	case ScreenLogin:
		return m.loginView()
	case ScreenSpending:
		return m.spendingView()
	case ScreenHelp:
		return helpView()
	default:
		return m.forecastContent(max(1, m.width-4))
	}
}

func (m Model) headerSubtitle() string {
	if m.authChecking {
		return "Connecting to Google APIs and validating your account before loading forecasts"
	}
	if m.onboarding {
		return "Google sign-in is required before the forecast workspace can open"
	}
	if l := m.selectedLocation(); l != nil {
		return fmt.Sprintf("%s   ·   %.4f, %.4f   ·   RUN %s", l.Name, l.Latitude, l.Longitude, initTime(m.currentProduct()))
	}
	return "No saved forecast loaded   ·   / to search for a location   ·   q to exit"
}

func (m Model) navigation() string {
	items := []struct {
		label  string
		active bool
	}{
		{"1  Forecast", m.screen == ScreenForecast && m.tab == TabHourly},
		{"2  Daily", m.screen == ScreenForecast && m.tab == TabDaily},
		{"3  Raw data", m.screen == ScreenForecast && m.tab == TabDetails},
		{"/  Places", m.screen == ScreenSearch},
		{"b  Billing", m.screen == ScreenSpending},
		{"s  Settings", m.screen == ScreenSettings},
		{"l  Login", m.screen == ScreenLogin},
		{"?  Help", m.screen == ScreenHelp},
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		label := item.label
		if item.active {
			label = "[ " + label + " ]"
			if !m.noColor {
				label = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#07151D")).Background(lipgloss.Color("#5EEAD4")).Render(label)
			}
		} else if !m.noColor {
			label = lipgloss.NewStyle().Foreground(lipgloss.Color("#A8B7C9")).Render(label)
		}
		parts = append(parts, label)
	}
	separator := "   "
	if m.width < 80 {
		parts = []string{"1 Forecast", "2 Day", "3 Raw", "/ Places", "b Bill", "s Setup", "l Login", "? Help"}
		separator = "  "
		if m.width < 50 {
			parts = []string{"1 Wx", "2 Day", "3 Raw", "/ Places", "b Bill", "s Setup", "l Login", "? Help"}
		}
	}
	return strings.Join(parts, separator)
}

func (m Model) keyHints(width int) string {
	if m.screen == ScreenSpending {
		return "↑/↓ scroll billing   Esc forecast   q quit"
	}
	if m.screen == ScreenForecast {
		if m.focusSidebar {
			if width < 58 {
				return "↑↓ place  d delete  → forecast"
			}
			return "↑↓ select place   → forecast   d delete   / add   r refresh"
		}
		if width < 58 {
			return "←→ time  ↑↓ data  h places"
		}
		if m.tab == TabDetails {
			return "↑↓ JSON lines   ←→ hour   h places   1 forecast   r refresh"
		}
		step := "period"
		if m.tab == TabDaily {
			step = "day"
		}
		return "←→ " + step + "   ↑↓ details   h places   3 raw   r refresh"
	}
	if width < 58 {
		return "Esc back   q quit"
	}
	return "Esc back   / places   b billing   s settings   q quit"
}

func (m Model) forecastTitle() string {
	switch m.tab {
	case TabDaily:
		return "DAILY OUTLOOK  /  LOCAL DATES"
	case TabDetails:
		return "RAW MODEL DATA  /  SELECTED HOUR"
	default:
		return "WEATHER AT A GLANCE  /  6-HOUR OUTLOOK"
	}
}

func (m Model) forecastContent(width int) string {
	switch m.tab {
	case TabDaily:
		return m.dailyView(width)
	case TabDetails:
		return m.detailsView(width)
	default:
		return m.summaryView(width)
	}
}

type periodForecast struct {
	rows       []map[string]any
	dateKey    string
	dateLabel  string
	name       string
	part       int
	startIndex int
	endIndex   int
	startHour  int
	endHour    int
	count      int
	temp       float64
	rainMM     float64
	cloudPct   float64
	windKPH    float64
	uWind      float64
	vWind      float64
}

func forecastPeriods(p *Product) []*periodForecast {
	if p == nil {
		return nil
	}
	periods := make([]*periodForecast, 0, len(p.Forecast)/4)
	periodIndex := make(map[string]*periodForecast)
	for index, row := range p.Forecast {
		ts, err := time.Parse(time.RFC3339, str(row["time"]))
		if err != nil {
			continue
		}
		local := ts.Local()
		dateKey := local.Format("2006-01-02")
		part := daypartIndex(local.Hour())
		key := fmt.Sprintf("%s/%d", dateKey, part)
		period := periodIndex[key]
		if period == nil {
			period = &periodForecast{
				dateKey: dateKey, dateLabel: local.Format("Mon 02 Jan"), name: daypartName(part), part: part,
				startIndex: index, endIndex: index, startHour: local.Hour(), endHour: local.Hour(),
			}
			periodIndex[key] = period
			periods = append(periods, period)
		}
		period.count++
		period.endIndex = index
		period.endHour = local.Hour()
		period.rows = append(period.rows, row)
	}
	for _, period := range periods {
		temp, _, _ := centralAggregate(period.rows, "temperature_2m")
		period.temp = tempValue(temp, "C")
		rain, count, _ := centralAggregate(period.rows, "total_precipitation_1hr")
		period.rainMM = rain * float64(count) * 1000
		cloud, _, _ := centralAggregate(period.rows, "total_cloud_cover")
		period.cloudPct = cloud * 100
		wind, _, _ := centralAggregate(period.rows, "wind_speed_10m")
		period.windKPH = wind * 3.6
		u, _, _ := centralAggregate(period.rows, "u_component_of_wind_10m")
		v, _, _ := centralAggregate(period.rows, "v_component_of_wind_10m")
		period.uWind, period.vWind = u*3.6, v*3.6

	}
	return periods
}

func daypartIndex(hour int) int {
	switch {
	case hour < 6:
		return 3
	case hour < 12:
		return 0
	case hour < 18:
		return 1
	default:
		return 2
	}
}

func daypartName(part int) string {
	return [...]string{"MORNING", "NOON", "EVENING", "NIGHT"}[clamp(part, 0, 3)]
}

func periodForHour(periods []*periodForecast, hour int) int {
	for i, period := range periods {
		if hour >= period.startIndex && hour <= period.endIndex {
			return i
		}
	}
	for i, period := range periods {
		if hour < period.startIndex {
			return i
		}
	}
	return max(0, len(periods)-1)
}

// Keep the page anchored while the selection moves across columns and rows.
// Paging starts only when the selection leaves the visible grid.
type forecastCardLayout struct {
	Columns, Start, End, CardHeight, DetailHeight, HeaderHeight int
	Compact                                                     bool
}

func (m Model) forecastBodySize() (int, int) {
	width := max(1, m.width)
	_, _, _, height := m.frameHeights()

	if width >= 100 {
		return width - 32, max(1, height-3)
	}
	if width < 72 && height >= 8 {
		sidebar := min(6, max(4, height/3))
		if height-sidebar >= 3 {
			height -= sidebar
		}
	}
	return width - 4, max(1, height-3)
}

func (m Model) forecastCardsLayout(width int, daily bool) forecastCardLayout {
	_, available := m.forecastBodySize()
	p := m.currentProduct()
	periods := forecastPeriods(p)
	total, selected := len(periods), periodForHour(periods, m.hour)
	layout := forecastCardLayout{Columns: min(4, max(1, width/26)), CardHeight: 12, HeaderHeight: 4}
	if daily {
		starts := forecastDayStarts(p)
		total, selected = len(starts), 0
		for i, start := range starts {
			if start <= m.hour {
				selected = i
			}
		}
		layout.CardHeight, layout.HeaderHeight = 14, 0
	}
	layout.Compact = available < layout.HeaderHeight+layout.CardHeight+8
	if layout.Compact {
		layout.CardHeight, layout.HeaderHeight = 5, 0
		if available < 10 {
			layout.CardHeight = 3
		}
	}
	rows := min(3, max(1, (available-layout.HeaderHeight-10)/layout.CardHeight))
	capacity := rows * layout.Columns
	layout.Start = selected / capacity * capacity
	layout.End = min(total, layout.Start+capacity)
	usedRows := (layout.End - layout.Start + layout.Columns - 1) / layout.Columns
	layout.DetailHeight = max(4, available-layout.HeaderHeight-usedRows*layout.CardHeight)
	return layout
}

func (m Model) summaryView(widths ...int) string {
	width, _ := m.forecastBodySize()
	if len(widths) > 0 {
		width = widths[0]
	}
	periods := forecastPeriods(m.currentProduct())
	if len(periods) == 0 {
		return "No cached forecast. Press / to add a place, or r to refresh a saved place."
	}
	active := periods[periodForHour(periods, m.hour)]
	layout := m.forecastCardsLayout(width, false)
	var b strings.Builder
	unit := "°" + m.unit()
	if layout.HeaderHeight > 0 {
		details := fmt.Sprintf("%s%s · rain total %.1f mm · wind %.0f km/h · cloud %.0f%%",
			formatTemperature(active.temp, m.unit()), unit, active.rainMM, active.windKPH, active.cloudPct)
		b.WriteString(m.panel(fmt.Sprintf("SELECTED · %s %s · %s", active.dateLabel, titleCase(active.name), weatherCondition(active)), details, width, layout.HeaderHeight, true) + "\n")
	}
	cardWidth := width / layout.Columns
	cards := make([]string, 0, layout.End-layout.Start)
	for _, period := range periods[layout.Start:layout.End] {
		description := weatherCondition(period)
		selected := m.hour >= period.startIndex && m.hour <= period.endIndex
		mark := "  "
		if selected {
			mark = "> "
		}
		title := mark + period.dateLabel + " " + titleCase(period.name)
		body := fmt.Sprintf("%s\n%s\n%s%s\nRain %.1fmm total\nWind %.0f km/h",
			m.weatherIllustration(description), description, formatTemperature(period.temp, m.unit()), unit, period.rainMM, period.windKPH)
		if layout.Compact {
			body = fmt.Sprintf("%s%s · %s\nRain %.1fmm", formatTemperature(period.temp, m.unit()), unit, description, period.rainMM)
		}
		if layout.CardHeight == 3 {
			body = ""
			title = mark + period.dateLabel + " " + formatTemperature(period.temp, m.unit()) + unit
		}
		cards = append(cards, m.panel(title, body, cardWidth, layout.CardHeight, selected))
	}
	for i := 0; i < len(cards); i += layout.Columns {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cards[i:min(len(cards), i+layout.Columns)]...) + "\n")
	}
	b.WriteString(m.readableDataCard(width, false))
	return b.String()
}

func (m Model) readableProduct(daily bool) *Product {
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return nil
	}
	start, end := 0, len(p.Forecast)
	if daily {
		starts := forecastDayStarts(p)
		for i, index := range starts {
			if index <= m.hour {
				start = index
				end = len(p.Forecast)
				if i+1 < len(starts) {
					end = starts[i+1]
				}
			}
		}
	} else {
		periods := forecastPeriods(p)
		if len(periods) > 0 {
			period := periods[periodForHour(periods, m.hour)]
			start, end = period.startIndex, period.endIndex+1
		}
	}
	return &Product{InitTime: p.InitTime, Forecast: p.Forecast[start:end]}
}

const centralEstimateNote = "Period mean of available hourly medians (P50)."

const dailyEstimateNote = "Daily mean of available hourly medians (P50)."

func (m Model) readableSpace(width int, daily bool) (visible int, note bool, footer bool) {
	layout := m.forecastCardsLayout(width, daily)
	visible = layout.DetailHeight - 3
	footerLines := lipgloss.Height(lipgloss.Wrap("Lines 9999–9999/9999 · ↑↓ scroll · 3 raw", max(1, width-4), " "))
	if visible >= footerLines+1 {
		footer = true
		visible -= footerLines
	}
	noteText := centralEstimateNote
	if daily {
		noteText = dailyEstimateNote
	}
	noteLines := lipgloss.Height(lipgloss.Wrap(noteText, max(1, width-4), " "))
	if visible >= noteLines+3 {
		note = true
		visible -= noteLines
	}
	return max(1, visible), note, footer
}
func (m Model) readableLineCapacity(width int, daily bool) int {
	visible, _, _ := m.readableSpace(width, daily)
	return visible
}

func (m Model) readableDataCard(width int, groupByDay bool) string {
	p := m.readableProduct(groupByDay)
	if p == nil || len(p.Forecast) == 0 {
		return ""
	}
	lines := readableForecastLines(p, width-4, m.unit(), groupByDay)
	visible, showNote, showFooter := m.readableSpace(width, groupByDay)
	start := clamp(m.readableScroll, 0, max(0, len(lines)-visible))
	end := min(len(lines), start+visible)
	body := strings.Join(lines[start:end], "\n")
	if showNote {
		noteText := centralEstimateNote
		if groupByDay {
			noteText = dailyEstimateNote
		}
		body = noteText + "\n" + body
	}
	if showFooter {
		body += fmt.Sprintf("\nLines %d–%d/%d · ↑↓ scroll · 3 raw", start+1, end, len(lines))
	}
	title := "COMPLETE READABLE FORECAST DATA"
	if groupByDay {
		title = "COMPLETE READABLE DAILY DATA"
	}
	return m.panel(title, body, width, m.forecastCardsLayout(width, groupByDay).DetailHeight, false)
}

func readableForecastLines(p *Product, width int, unit string, groupByDay bool) []string {
	if p == nil {
		return nil
	}
	return readableAggregateLines(p, width, unit, groupByDay)
}

func humanFieldName(name string) string {
	words := strings.Fields(strings.ReplaceAll(name, "_", " "))
	for i, word := range words {
		switch strings.ToLower(word) {
		case "p10", "p50", "p90":
			words[i] = strings.ToUpper(word)
		case "2m", "10m", "100m":
			words[i] = word[:len(word)-1] + " m"
		case "1hr":
			words[i] = "1 hr"
		default:
			words[i] = titleCase(word)
		}
	}
	return strings.Join(words, " ")
}

func (m Model) readableVisibleLines() int {
	width, _ := m.forecastBodySize()
	return m.readableLineCapacity(width, m.tab == TabDaily)
}

func (m Model) readableScrollMax() int {
	p := m.readableProduct(m.tab == TabDaily)
	if p == nil {
		return 0
	}
	width, _ := m.forecastBodySize()
	lines := readableForecastLines(p, width-4, m.unit(), m.tab == TabDaily)
	return max(0, len(lines)-m.readableVisibleLines())
}

func weatherCondition(period *periodForecast) string {
	if period == nil {
		return "Unknown"
	}
	averageRain := period.rainMM / float64(max(1, period.count))
	switch {
	case averageRain >= 1.0:
		return "Rain"
	case averageRain >= 0.15:
		return "Showers"
	case period.cloudPct < 15:
		return "Sunny"
	case period.cloudPct < 45:
		return "Partly cloudy"
	case period.cloudPct < 80:
		return "Mostly cloudy"
	case math.IsNaN(period.cloudPct):
		return "Unavailable"
	default:
		return "Overcast"
	}
}

func weatherArt(description string) [5]string {
	switch description {
	case "Sunny":
		return [5]string{`    \   |   /`, `     .---.`, ` -- (     ) --`, `     '---'`, `    /   |   \`}
	case "Partly cloudy":
		return [5]string{`   \  /`, ` _ /"".-.`, `   \_(   ).`, `   /(___(__)`, `             `}
	case "Mostly cloudy", "Overcast":
		return [5]string{`      .--.`, `   .-(    ).`, `  (___.__)__)`, `             `, `             `}
	case "Showers", "Rain":
		return [5]string{`      .--.`, `   .-(    ).`, `  (_________)`, `    / / / /`, `   / / / /`}
	default:
		return [5]string{`      .--.`, `   .-(    ).`, `  (___.__)__)`, `             `, `             `}
	}
}

func (m Model) weatherIllustration(description string) string {
	art := weatherArt(description)
	if m.noColor {
		return strings.Join(art[:], "\n")
	}
	for i, line := range art {
		color := "#CBD5E1"
		if description == "Sunny" || (description == "Partly cloudy" && i < 2) {
			color = "#FACC15"
		}
		if (description == "Rain" || description == "Showers") && i >= 3 {
			color = "#7DD3FC"
		}
		art[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(line)
	}
	return strings.Join(art[:], "\n")
}

func shortCondition(s string) string {
	switch s {
	case "Partly cloudy":
		return "Partly"
	case "Mostly cloudy":
		return "Cloudy"
	case "Showers":
		return "Showers"
	default:
		return s
	}
}

func formatTemperature(celsius float64, unit string) string {
	value := temperatureInUnit(celsius, unit)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "—"
	}
	return fmt.Sprintf("%+.0f", value)
}

func temperatureInUnit(celsius float64, unit string) float64 {
	if unit == "F" {
		return celsius*9/5 + 32
	}
	return celsius
}

func formatRange(low, high float64, unit string) string {
	low, high = temperatureInUnit(low, unit), temperatureInUnit(high, unit)
	if math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) {
		return "—"
	}
	suffix := "°"
	if unit != "" {
		suffix += unit
	}
	return fmt.Sprintf("%.0f–%.0f%s", low, high, suffix)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func windArrow(u, v float64) string {
	if math.IsNaN(u) || math.IsNaN(v) || (u == 0 && v == 0) {
		return "·"
	}
	angles := []string{"→", "↘", "↓", "↙", "←", "↖", "↑", "↗"}
	index := int(math.Round(math.Mod(math.Atan2(-v, u)+2*math.Pi, 2*math.Pi)/(math.Pi/4))) % len(angles)
	return angles[index]
}

func (m Model) panel(title, content string, width, height int, focused bool) string {
	width, height = max(8, width), max(3, height)
	innerWidth, innerHeight := width-4, height-2
	title = fitLine(title, innerWidth)
	if !m.noColor {
		color := "#A8B7C9"
		if focused {
			color = "#5EEAD4"
		}
		title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(title)
	}
	rows := []string{title}
	if strings.TrimSpace(content) != "" {
		wrapped := lipgloss.Wrap(strings.TrimRight(content, "\n"), innerWidth, " ")
		rows = append(rows, strings.Split(wrapped, "\n")...)
	}
	if len(rows) > innerHeight {
		rows = rows[:innerHeight]
		if innerHeight > 2 {
			rows[innerHeight-1] = fitLine("… more below", innerWidth)
		}
	}
	for len(rows) < innerHeight {
		rows = append(rows, "")
	}
	border := lipgloss.RoundedBorder()
	if m.useASCII {
		border = lipgloss.ASCIIBorder()
	}
	style := lipgloss.NewStyle().Width(width).Height(height).Padding(0, 1).BorderStyle(border)
	if !m.noColor {
		color := "#34465C"
		if focused {
			color = "#5EEAD4"
		}
		style = style.BorderForeground(lipgloss.Color(color))
	}
	return style.Render(strings.Join(rows, "\n"))
}

func fitLine(s string, width int) string {
	if width < 1 || lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > width-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	b.WriteRune('…')
	return b.String()
}

func (m Model) hourlyView(widths ...int) string {
	width := m.width - 8
	if len(widths) > 0 {
		width = widths[0]
	}
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No cached forecast. Press r to query after Google Cloud setup."
	}
	limit := min(len(p.Forecast), max(1, m.height-11))
	start := clamp(m.hour-limit/2, 0, max(0, len(p.Forecast)-limit))
	var b strings.Builder
	if width >= 72 {
		if m.useASCII {
			b.WriteString("TIME          TEMP C (P10-P90)     RAIN mm   CLOUD   WIND km/h\n")
		} else {
			b.WriteString("TIME          TEMP °C (P10–P90)    RAIN mm   CLOUD   WIND km/h\n")
		}
	} else {
		b.WriteString("TIME       TEMP p10–p90       RAIN  CLOUD  WIND\n")
	}
	for i := start; i < start+limit; i++ {
		row := p.Forecast[i]
		t := localTime(str(row["time"]))
		temp := tempValue(number(row["temperature_2m_mean"]), m.unit())
		p10 := tempValue(number(row["temperature_2m_p10"]), m.unit())
		p90 := tempValue(number(row["temperature_2m_p90"]), m.unit())
		rain := number(row["total_precipitation_1hr_mean"]) * 1000
		cloud := number(row["total_cloud_cover_mean"]) * 100
		wind := number(row["wind_speed_10m_mean"]) * 3.6
		cursor := " "
		if i == m.hour {
			cursor = ">"
		}
		if width >= 72 {
			b.WriteString(fmt.Sprintf("%s %-12s %5.1f (%4.1f–%4.1f) %7.2f   %3.0f%%    %5.1f\n", cursor, t, temp, p10, p90, rain, cloud, wind))
		} else {
			b.WriteString(fmt.Sprintf("%s %-9s %5.1f %4.1f–%4.1f %6.2f %3.0f%% %5.1f\n", cursor, t, temp, p10, p90, rain, cloud, wind))
		}
	}
	b.WriteString("\nPercentile ranges describe ensemble spread; they are not a calibrated confidence score.\n")
	return b.String()
}

type daySummary struct {
	rows                            []map[string]any
	date                            string
	count                           int
	tempSum, minTemp, maxTemp, rain float64
	rainCount                       int
	cloudSum, windSum               float64
	startIndex, endIndex            int
}

func (m Model) dailyView(widths ...int) string {
	width := m.width - 8
	if len(widths) > 0 {
		width = widths[0]
	}
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No cached forecast. Press r to query after Google Cloud setup."
	}
	days := map[string]*daySummary{}
	for i, row := range p.Forecast {
		ts, err := time.Parse(time.RFC3339, str(row["time"]))
		if err != nil {
			continue
		}
		d := ts.Local().Format("2006-01-02")
		s := days[d]
		if s == nil {
			s = &daySummary{date: d, startIndex: i}
			days[d] = s
		}
		s.count++
		s.endIndex = i
		s.rows = append(s.rows, row)
	}
	keys := make([]string, 0, len(days))
	for k, d := range days {
		temp, _, _ := centralAggregate(d.rows, "temperature_2m")
		d.tempSum = tempValue(temp, "C") * float64(d.count)
		d.minTemp, d.maxTemp = math.NaN(), math.NaN()
		values, _ := centralSamples(d.rows, "temperature_2m")
		for i, n := range values {
			c := tempValue(n, "C")
			if i == 0 || c < d.minTemp {
				d.minTemp = c
			}
			if i == 0 || c > d.maxTemp {
				d.maxTemp = c
			}
		}
		rain, count, _ := centralAggregate(d.rows, "total_precipitation_1hr")
		d.rain, d.rainCount = rain*float64(count)*1000, count
		cloud, _, _ := centralAggregate(d.rows, "total_cloud_cover")
		wind, _, _ := centralAggregate(d.rows, "wind_speed_10m")
		d.cloudSum = cloud * 100 * float64(d.count)
		d.windSum = wind * 3.6 * float64(d.count)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "No valid daily forecast entries."
	}
	layout := m.forecastCardsLayout(width, true)
	cardWidth := width / layout.Columns
	cards := make([]string, 0, layout.End-layout.Start)
	for _, key := range keys[layout.Start:layout.End] {
		d := days[key]
		avg := d.tempSum / float64(d.count)
		period := &periodForecast{count: d.count, rainMM: d.rain, cloudPct: d.cloudSum / float64(max(1, d.count))}
		description := weatherCondition(period)
		date, _ := time.Parse("2006-01-02", d.date)
		selected := m.hour >= d.startIndex && m.hour <= d.endIndex
		mark := "  "
		if selected {
			mark = "> "
		}
		title := mark + date.Format("Mon 02 Jan")
		body := fmt.Sprintf("%s\n%s\nAvg %s°%s\nLow/high %.0f/%.0f°\nRain %.1f mm total\nWind %.0f km/h\nCloud %.0f%%",
			m.weatherIllustration(description), description, formatTemperature(avg, m.unit()), m.unit(),
			temperatureInUnit(d.minTemp, m.unit()), temperatureInUnit(d.maxTemp, m.unit()), d.rain,
			d.windSum/float64(max(1, d.count)), d.cloudSum/float64(max(1, d.count)))
		if layout.Compact {
			body = fmt.Sprintf("Avg %s°%s · %s\nRain %.1fmm", formatTemperature(avg, m.unit()), m.unit(), description, d.rain)
		}
		if layout.CardHeight == 3 {
			body = ""
			title = mark + date.Format("Mon 02 Jan") + " " + formatTemperature(avg, m.unit()) + "°" + m.unit()
		}
		cards = append(cards, m.panel(title, body, cardWidth, layout.CardHeight, selected))
	}
	var b strings.Builder
	for i := 0; i < len(cards); i += layout.Columns {
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cards[i:min(len(cards), i+layout.Columns)]...) + "\n")
	}
	b.WriteString(m.readableDataCard(width, true))
	return b.String()
}

func (m Model) detailsView(widths ...int) string {
	width := m.width - 8
	if len(widths) > 0 {
		width = widths[0]
	}
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No forecast details available."
	}
	i := clamp(m.hour, 0, len(p.Forecast)-1)
	row := p.Forecast[i]
	lines := rawJSONLines(row, width)
	visible := m.detailVisibleLines()
	start := clamp(m.detailScroll, 0, max(0, len(lines)-visible))
	end := min(len(lines), start+visible)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("RAW JSON · %s · HOUR %d/%d\n", localTime(str(row["time"])), i+1, len(p.Forecast)))
	if end > start {
		b.WriteString(strings.Join(lines[start:end], "\n"))
	}
	if len(lines) > 0 {
		b.WriteString(fmt.Sprintf("\nJSON lines %d–%d of %d · ↑/↓ scroll · ←/→ change hour\n", start+1, end, len(lines)))
	}
	return b.String()
}

func rawJSONLines(row map[string]any, width int) []string {
	raw, err := json.MarshalIndent(row, "", "  ")
	if err != nil {
		return []string{"{ }"}
	}
	lines := strings.Split(string(raw), "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(lipgloss.Wrap(line, max(1, width), ""), "\n")...)
	}
	return wrapped
}

func (m Model) detailVisibleLines() int {
	return max(1, m.height-18)
}

func (m Model) detailLineCount() int {
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return 0
	}
	width := m.width - 4
	if m.width >= 100 {
		width = m.width - 32
	}
	row := p.Forecast[clamp(m.hour, 0, len(p.Forecast)-1)]
	return len(rawJSONLines(row, width))
}

func (m Model) detailScrollMax() int {
	return max(0, m.detailLineCount()-m.detailVisibleLines())
}

func (m Model) spendingDocument() string {
	var b strings.Builder
	b.WriteString("SPENDING & BIGQUERY JOBS\n\n")
	if len(m.spending) > 0 {
		var summary struct {
			BytesBilled *int64   `json:"reported_bytes_billed"`
			Estimate    *float64 `json:"estimated_cost_usd"`
			Rate        *float64 `json:"estimate_rate_usd_per_tib"`
			DailyBytes  *int64   `json:"daily_bytes_billed"`
			Monthly     *int64   `json:"monthly_bytes_billed"`
			DailyLimit  *int64   `json:"daily_byte_limit"`
			MonthLimit  *int64   `json:"monthly_byte_limit"`
		}
		if json.Unmarshal(m.spending, &summary) == nil {
			b.WriteString("BIGQUERY COST ESTIMATE\n")
			if summary.BytesBilled == nil {
				b.WriteString("Billed bytes: unavailable\nEstimated cost: unavailable until BigQuery reports billed bytes.\n")
			} else {
				b.WriteString(fmt.Sprintf("Billed bytes: %s\n", formatBytes(*summary.BytesBilled)))
				// Use the published standard on-demand rate unless the user has set
				// an account-specific rate in Settings. One TiB is 2^40 bytes.
				rate := 6.25
				rateLabel := "default on-demand rate"
				if summary.Rate != nil {
					rate = *summary.Rate
					rateLabel = "configured rate"
				}
				estimate := float64(*summary.BytesBilled) * rate / float64(uint64(1)<<40)
				if summary.Rate != nil && summary.Estimate != nil {
					estimate = *summary.Estimate
				}
				b.WriteString(fmt.Sprintf("Estimated cost: $%.6f USD · $%.2f/TiB %s\n", estimate, rate, rateLabel))
				if summary.Rate == nil {
					b.WriteString("Gross on-demand estimate; account free tier, credits, reservations, and taxes can change actual charges. Set USD per TiB in Settings to override.\n")
				}
			}
			if summary.DailyBytes != nil {
				b.WriteString(fmt.Sprintf("Today: %s", formatBytes(*summary.DailyBytes)))
				if summary.DailyLimit != nil {
					b.WriteString(fmt.Sprintf(" / %s limit", formatBytes(*summary.DailyLimit)))
				}
				b.WriteByte('\n')
			}
			if summary.Monthly != nil {
				b.WriteString(fmt.Sprintf("This month: %s", formatBytes(*summary.Monthly)))
				if summary.MonthLimit != nil {
					b.WriteString(fmt.Sprintf(" / %s limit", formatBytes(*summary.MonthLimit)))
				}
				b.WriteByte('\n')
			}
			b.WriteString("\nLEDGER DETAILS\n" + prettyJSON(m.spending) + "\n")
		} else {
			b.WriteString(prettyJSON(m.spending) + "\n")
		}
	} else {
		b.WriteString("No spending summary loaded.\n")
	}
	if len(m.jobs) > 0 {
		b.WriteString("\nRECENT JOBS\n" + prettyJSON(m.jobs))
	}
	b.WriteString("\n\nBilled bytes come from BigQuery job metadata. Cost is an estimate from billed bytes and the configured USD/TiB rate; cache hits bill zero.\n")
	return b.String()
}

func (m Model) spendingView() string {
	width := max(1, m.width-4)
	_, _, _, mainHeight := m.frameHeights()
	visible := max(1, mainHeight-3) // Workspace frame title and borders.
	lines := strings.Split(lipgloss.Wrap(strings.TrimRight(m.spendingDocument(), "\n"), width, " "), "\n")
	start := clamp(m.spendingScroll, 0, max(0, len(lines)-visible))
	end := min(len(lines), start+visible)
	return strings.Join(lines[start:end], "\n")
}

func (m Model) spendingWindow() (start, end, total int) {
	width := max(1, m.width-4)
	_, _, _, mainHeight := m.frameHeights()
	visible := max(1, mainHeight-3)
	lines := strings.Split(lipgloss.Wrap(strings.TrimRight(m.spendingDocument(), "\n"), width, " "), "\n")
	start = clamp(m.spendingScroll, 0, max(0, len(lines)-visible))
	return start, min(len(lines), start+visible), len(lines)
}

func (m *Model) scrollSpending(delta int) {
	start, _, total := m.spendingWindow()
	_, _, _, mainHeight := m.frameHeights()
	visible := max(1, mainHeight-3)
	m.spendingScroll = clamp(start+delta, 0, max(0, total-visible))
}
func (m Model) searchView() string {
	var b strings.Builder
	b.WriteString("SEARCH LOCATIONS\n\nPlace name: " + m.search + "_\n\nEnter searches Google Maps; ↑/↓ selects a result; Enter saves it.\n")
	for i, c := range m.candidates {
		mark := "  "
		if i == m.candidate {
			mark = "> "
		}
		b.WriteString(fmt.Sprintf("%s%s  (%.5f, %.5f) · %s\n", mark, c.Name, c.Latitude, c.Longitude, attributionLabel(c.Attribution)))
	}
	if len(m.candidates) == 0 {
		b.WriteString("Search is sent only when you press Enter.\n")
	}
	if len(m.candidates) > 0 {
		b.WriteString("Search results provided by Google Maps.\n")
	}
	return b.String()
}

func (m Model) settingsView() string {
	values := m.settingsValues
	if values == nil {
		values = map[string]any{}
	}
	rows := settingRows()
	var b strings.Builder
	b.WriteString("SETTINGS · values are local to this machine\n\n")
	b.WriteString("WEATHERNEXT BIGQUERY TARGET · required before forecasts can open\n")
	if m.targetProjectHint != "" {
		b.WriteString("Google credentials suggest project " + m.targetProjectHint +
			" · select Project ID and press Ctrl+P to copy; access still needs verification.\n")
	}
	b.WriteString("Enter a project ID, dataset name, and table name. The app checks table metadata before any forecast query.\n\n")
	if m.mapsConfigured {
		b.WriteString("Google Maps: configured automatically · Geocoding API only\n\n")
	} else {
		b.WriteString("Google Maps: automatic setup pending or unavailable · Ctrl+G retries setup\n\n")
	}
	if m.mapsSetupMessage != "" {
		b.WriteString(m.mapsSetupMessage + "\n\n")
	}
	for i, row := range rows {
		mark := "  "
		if i == m.settingIndex {
			mark = "> "
		}
		v := values[row.key]
		shown := fmt.Sprint(v)
		if v == nil || shown == "<nil>" {
			shown = "unset"
		}
		if i == m.settingIndex && m.settingsEdit != "" {
			shown = m.settingsEdit + "_"
		}
		b.WriteString(fmt.Sprintf("%s%-60s %s\n", mark, row.label, shown))
	}
	mark := "  "
	if m.settingIndex == len(rows) {
		mark = "> "
	}
	keyText := "unset"
	if m.mapsKey != "" {
		keyText = strings.Repeat("•", len(m.mapsKey))
	}
	b.WriteString(fmt.Sprintf("%s%-60s %s\n", mark, "Optional Maps API key override (session only)", keyText))
	b.WriteString("\n↑/↓ selects · type value · Enter saves · Ctrl+T checks target access · Ctrl+P copies project suggestion\n")
	b.WriteString("Use 'off' to clear a target value. Target names are never filled from Python defaults.\n")
	b.WriteString("Ctrl+G configures Maps · c/f sets temperature unit · Esc returns\n")
	b.WriteString("Daily/monthly byte thresholds stop the app before a later job after reported billed bytes reach the threshold; they do not cap active queries or other Google Cloud usage.\n")
	b.WriteString("Automatic setup enables the Geocoding and API Keys services, then stores a restricted key in local credentials.\n")
	return b.String()
}

func settingRows() []struct{ key, label string } {
	return []struct{ key, label string }{
		{"target_project", "Google Cloud project ID (query and target project)"},
		{"target_dataset", "WeatherNext BigQuery dataset name"},
		{"target_table", "WeatherNext BigQuery table name"},
		{"default_hours", "Forecast hours (1–360)"},
		{"max_bytes_billed", "Per-query maximum billed bytes; blank/unset means no cap"},
		{"max_daily_billed_bytes", "Daily billed-byte threshold (UTC); unset means no threshold"},
		{"max_monthly_billed_bytes", "Monthly billed-byte threshold (UTC); unset means no threshold"},
		{"units", "Temperature units (press c or f to choose)"},
		{"usd_per_tib", "Optional BigQuery USD per TiB estimate rate"},
		{"maps_usd_per_1000_requests", "Optional Maps USD per 1,000 requests"},
	}
}

func attributionLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Google Maps"
	}
	return value
}

func isByteLimitField(field string) bool {
	return field == "max_bytes_billed" || field == "max_daily_billed_bytes" || field == "max_monthly_billed_bytes"
}

func isTargetField(field string) bool {
	return field == "target_project" || field == "target_dataset" || field == "target_table"
}

func (m Model) settingField() string {
	rows := settingRows()
	if m.settingIndex < 0 || m.settingIndex >= len(rows) {
		return ""
	}
	return rows[m.settingIndex].key
}

func (m Model) hasTargetConfig() bool {
	if m.settingsValues == nil {
		return false
	}
	for _, key := range []string{"target_project", "target_dataset", "target_table"} {
		value, ok := m.settingsValues[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func (m Model) targetParams() map[string]any {
	settings := m.settingsValues
	if settings == nil {
		settings = map[string]any{}
	}
	return map[string]any{
		"project": settings["target_project"],
		"dataset": settings["target_dataset"],
		"table":   settings["target_table"],
	}
}

func (m Model) loginView() string {
	var state struct {
		Authenticated bool   `json:"authenticated"`
		Connected     bool   `json:"connected"`
		RequiresLogin bool   `json:"requires_login"`
		AccountHint   string `json:"account_hint"`
		TargetProject string `json:"target_project"`
		TargetDataset string `json:"target_dataset"`
		Table         string `json:"table"`
		Message       string `json:"message"`
	}
	_ = json.Unmarshal(m.login, &state)
	b := strings.Builder{}
	if m.onboarding {
		b.WriteString("WELCOME TO WEATHERNEXT 3\n\n")
		b.WriteString("Google sign-in and WeatherNext table access are checked before any saved forecast opens.\n\n")
	} else {
		b.WriteString("GOOGLE CLOUD ACCESS\n\n")
	}
	b.WriteString("GOOGLE CREDENTIALS\n")
	switch {
	case m.authChecking:
		b.WriteString("  Checking the selected Google account…\n")
	case state.Authenticated:
		if state.AccountHint != "" {
			b.WriteString("  Credentials found · " + state.AccountHint + "\n")
		} else {
			b.WriteString("  Credentials found\n")
		}
	case state.RequiresLogin:
		b.WriteString("  Sign-in required\n")
	default:
		b.WriteString("  Not verified yet\n")
	}
	targetProject, targetDataset, targetTable := m.targetProject, "", m.targetTable
	if m.settingsValues != nil {
		if value, ok := m.settingsValues["target_project"].(string); ok {
			targetProject = value
		}
		if value, ok := m.settingsValues["target_dataset"].(string); ok {
			targetDataset = value
		}
		if value, ok := m.settingsValues["target_table"].(string); ok {
			targetTable = value
		}
	}
	if state.TargetProject != "" {
		targetProject = state.TargetProject
	}
	if state.Table != "" {
		targetTable = strings.TrimPrefix(state.Table, targetProject+"."+targetDataset+".")
	}
	if targetProject != "" || targetDataset != "" || targetTable != "" {
		b.WriteString("\nWEATHERNEXT TABLE\n")
		b.WriteString("  Project: " + settingValue(targetProject) + "\n")
		b.WriteString("  Dataset: " + settingValue(targetDataset) + "\n")
		b.WriteString("  Table:   " + settingValue(targetTable) + "\n")
		switch {
		case state.Connected:
			b.WriteString("  Access:  verified\n")
		case m.authChecking:
			b.WriteString("  Access:  checking metadata only (no forecast query)\n")
		default:
			b.WriteString("  Access:  not verified\n")
		}
	}
	if m.authChecking {
		b.WriteString("\nConnecting to Google APIs… forecast data is held until this check succeeds.\n")
	}
	if m.mapsSettingUp {
		b.WriteString("\nCreating or checking a Geocoding-only Maps key…\n")
	}
	if m.loginMessage != "" {
		b.WriteString("\n" + m.loginMessage + "\n")
	} else if state.Message != "" {
		b.WriteString("\n" + state.Message + "\n")
	}
	if m.loginFlow {
		b.WriteString("\nPaste the one-time Google code here: " + strings.Repeat("*", len(m.loginCode)) + "_\nEnter submits · o opens verification URL · Esc cancels the login\n")
	} else if !state.Authenticated {
		b.WriteString("\nPress a to start Google sign-in. Open the verification URL, approve access, then paste the returned code here.\n")
	} else if !m.hasTargetConfig() {
		b.WriteString("\nPress s to configure the project ID, dataset, and table in Settings. Credentials do not contain shared table names.\n")
	} else if !state.Connected {
		b.WriteString("\nPress s to edit the target or a to switch Google accounts. A missing table and a permission denial are shown separately.\n")
	} else {
		b.WriteString("\nGoogle access is verified. Forecast data is available in the workspace.\n")
	}
	b.WriteString("Credentials are handled by gcloud Application Default Credentials. Codes are held in memory only.\n")
	return b.String()
}

func settingValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return value
}

func openVerificationURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !(u.Hostname() == "google.com" || strings.HasSuffix(u.Hostname(), ".google.com")) {
		return fmt.Errorf("refusing unexpected Google verification URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u.String())
	case "darwin":
		cmd = exec.Command("open", u.String())
	default:
		cmd = exec.Command("xdg-open", u.String())
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}
func sectionView(title string, raw json.RawMessage, empty string) string {
	if len(raw) == 0 {
		return title + "\n\n" + empty
	}
	return title + "\n\n" + prettyJSON(raw) + "\n\n" + empty
}
func helpView() string {
	return "KEYS\n\n↑ / k       previous location or hour\n↓ / j       next location or hour\n← / h       select location sidebar\n→           return focus to forecast\nTab         next forecast tab\n1 / 2 / 3   hourly / daily / details\n/           search locations; Enter searches, arrows select candidates\nr           fetch selected location for configured hours (BigQuery may bill)\nb           billed-byte ledger and job history\ns           settings: Google project/dataset/table, units, hours, byte caps, rates\nCtrl+T      check configured BigQuery table metadata; no forecast query\nCtrl+P      copy credential project suggestion into the selected Project ID field\nl / a      login status / start login; Esc cancels, code stays masked\nEsc         back or cancel current operation\nq           quit\n\nThe app requires an explicit target table. Credentials can suggest a project ID, but do not identify a shared WeatherNext dataset or table. Forecasts open from local JSON offline; refresh sends a BigQuery request.\n"
}

func (m Model) selectedLocation() *Location {
	if m.selected < 0 || m.selected >= len(m.data.Locations) {
		return nil
	}
	return &m.data.Locations[m.selected]
}
func (m Model) currentProduct() *Product {
	l := m.selectedLocation()
	if l == nil {
		return nil
	}
	if p, ok := l.Products["surface"]; ok {
		return &p
	}
	for _, p := range l.Products {
		return &p
	}
	return nil
}
func (m Model) hourCount() int {
	p := m.currentProduct()
	if p == nil {
		return 0
	}
	return len(p.Forecast)
}
func (m Model) unit() string {
	if v, ok := m.settingsValues["units"].(string); ok && strings.EqualFold(v, "F") {
		return "F"
	}
	return "C"
}
func tempValue(k float64, unit string) float64 {
	if unit == "F" {
		return k*9/5 - 459.67
	}
	return k - 273.15
}
func displayField(name string, v any, unit string) string {
	n := number(v)
	switch {
	case strings.Contains(name, "temperature"):
		n = tempValue(n, unit)
	case strings.Contains(name, "wind_speed") || strings.Contains(name, "component_of_wind"):
		n *= 3.6
	case strings.Contains(name, "precipitation") || strings.Contains(name, "_tp_"):
		n *= 1000
	case strings.Contains(name, "cloud_cover"):
		n *= 100
	case strings.Contains(name, "mean_sea_level_pressure"):
		n /= 100
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "not available"
	}
	if strings.Contains(name, "precipitation") || strings.Contains(name, "_tp_") {
		if math.Abs(n) < 0.005 {
			n = 0
		}
		return fmt.Sprintf("%.2f", n)
	}
	if math.Abs(n) < 0.05 {
		n = 0
	}
	return fmt.Sprintf("%.1f", n)
}
func fieldUnit(name, unit string) string {
	switch {
	case strings.Contains(name, "temperature"):
		return "°" + unit
	case strings.Contains(name, "wind_speed") || strings.Contains(name, "component_of_wind"):
		return "km/h"
	case strings.Contains(name, "precipitation") || strings.Contains(name, "_tp_"):
		return "mm"
	case strings.Contains(name, "cloud_cover"):
		return "%"
	case strings.Contains(name, "mean_sea_level_pressure"):
		return "hPa"
	}
	return ""
}
func (m *Model) decodeSettings(raw json.RawMessage) {
	var result struct {
		Settings    map[string]any `json:"settings"`
		Billing     map[string]any `json:"billing"`
		ProjectHint string         `json:"project_hint"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return
	}
	if m.settingsValues == nil {
		m.settingsValues = map[string]any{}
	}
	for k, v := range result.Settings {
		m.settingsValues[k] = v
	}
	for k, v := range result.Billing {
		m.settingsValues[k] = v
	}
	if result.ProjectHint != "" {
		m.targetProjectHint = result.ProjectHint
	}
	if _, ok := m.settingsValues["default_hours"]; !ok {
		m.settingsValues["default_hours"] = 24
	}
	if _, ok := m.settingsValues["units"]; !ok {
		m.settingsValues["units"] = "C"
	}
}
func (m *Model) saveSettings() tea.Cmd {
	if m.settingsValues == nil {
		m.settingsValues = map[string]any{}
	}
	fields := settingRows()
	if m.settingIndex < 0 || m.settingIndex >= len(fields) {
		return nil
	}
	field := fields[m.settingIndex].key
	if field != "units" {
		input := strings.TrimSpace(m.settingsEdit)
		if input == "" {
			m.status = "Type a value, or 'off' to clear it, then press Enter to save"
			return nil
		}
		if strings.EqualFold(input, "off") && isTargetField(field) {
			m.settingsValues[field] = ""
		} else if strings.EqualFold(input, "off") {
			m.settingsValues[field] = nil
		} else if field == "default_hours" || isByteLimitField(field) {
			v, err := strconv.ParseInt(input, 10, 64)
			if err != nil || v <= 0 || field == "default_hours" && v > 360 {
				m.status = "Enter a positive integer (hours: 1–360) or 'off' to unset a byte threshold"
				return nil
			}
			m.settingsValues[field] = v
		} else if isTargetField(field) {
			m.settingsValues[field] = input
		} else {
			v, err := strconv.ParseFloat(input, 64)
			if err != nil || v < 0 {
				m.status = "Enter a non-negative USD rate"
				return nil
			}
			m.settingsValues[field] = v
		}
		m.settingsEdit = ""
	}
	settings := map[string]any{}
	for _, k := range []string{"target_project", "target_dataset", "target_table", "default_hours", "max_bytes_billed", "max_daily_billed_bytes", "max_monthly_billed_bytes", "units"} {
		if v, ok := m.settingsValues[k]; ok {
			settings[k] = v
		}
	}
	billing := map[string]any{}
	for _, k := range []string{"usd_per_tib", "maps_usd_per_1000_requests"} {
		if v, ok := m.settingsValues[k]; ok {
			billing[k] = v
		}
	}
	return m.run("settings.update", map[string]any{"settings": settings, "billing": billing})
}
func (m Model) forecastHours() int {
	if v, ok := m.settingsValues["default_hours"]; ok {
		switch n := v.(type) {
		case float64:
			if n >= 1 && n <= 360 {
				return int(n)
			}
		case int:
			if n >= 1 && n <= 360 {
				return n
			}
		case int64:
			if n >= 1 && n <= 360 {
				return int(n)
			}
		}
	}
	return 24
}
func initTime(p *Product) string {
	if p == nil {
		return "unknown run"
	}
	t, e := time.Parse(time.RFC3339, p.InitTime)
	if e != nil {
		return p.InitTime
	}
	return t.UTC().Format("02 Jan 15:04Z")
}
func localTime(s string) string {
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		return s
	}
	return t.Local().Format("Mon 02 Jan 15:04")
}
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return math.NaN()
}
func displayNumber(v any) string {
	n := number(v)
	if math.IsNaN(n) {
		return fmt.Sprint(v)
	}
	return fmt.Sprintf("%.4f", n)
}
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func prettyJSON(raw json.RawMessage) string {
	var x any
	if json.Unmarshal(raw, &x) != nil {
		return string(raw)
	}
	b, e := json.MarshalIndent(x, "", "  ")
	if e != nil {
		return string(raw)
	}
	return string(b)
}
func formatBytes(n int64) string {
	const gib = 1024 * 1024 * 1024
	const tib = gib * 1024
	if n >= tib {
		return fmt.Sprintf("%.3f TiB", float64(n)/tib)
	}
	if n >= gib {
		return fmt.Sprintf("%.3f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d bytes", n)
}
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func truncate(s string, n int) string {
	if len(s) > n {
		if n < 4 {
			return s[:n]
		}
		return s[:n-1] + "…"
	}
	return s
}

func supportsUnicode() bool {
	if os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != "" {
		return true
	}
	locale := strings.ToLower(os.Getenv("LC_ALL") + os.Getenv("LC_CTYPE") + os.Getenv("LANG"))
	return strings.Contains(locale, "utf-8") || strings.Contains(locale, "utf8")
}
func asciiFallback(s string) string {
	r := strings.NewReplacer("↑", "^", "↓", "v", "←", "<", "→", ">", "│", "|", "·", "|", "°", " deg", "–", "-", "—", "-", "…", "...", "•", "*")
	return r.Replace(s)
}
