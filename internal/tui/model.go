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
	data           Data
	caller         Caller
	forecastPath   string
	screen         Screen
	tab            Tab
	selected       int
	hour           int
	width, height  int
	search         string
	candidates     []Location
	candidate      int
	loginCode      string
	loginFlow      bool
	loginMessage   string
	loginURL       string
	loginRequestID string
	mapsKey        string
	focusSidebar   bool
	status         string
	loading        bool
	busy           string
	spending       json.RawMessage
	jobs           json.RawMessage
	settings       json.RawMessage
	settingsValues map[string]any
	settingIndex   int
	settingsEdit   string
	importQueue    []Location
	autoImported   bool
	login          json.RawMessage
	useASCII       bool
	noColor        bool
	requestCancel  context.CancelFunc
	eventCtx       context.Context
	eventCh        chan backendEvent
}

func New(raw []byte, caller Caller) Model {
	return NewWithPath(raw, caller, "output/forecast.json")
}

func NewWithPath(raw []byte, caller Caller, path string) Model {
	m := Model{caller: caller, forecastPath: path, width: 80, height: 24, noColor: os.Getenv("NO_COLOR") != ""}
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
		return m.run("forecast.open", map[string]any{"path": m.forecastPath})
	}
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
			m.status = msg.err.Error()
			return m, nil
		}
		switch msg.method {
		case "forecast.fetch", "forecast.open":
			var result struct {
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal(msg.data, &result) == nil && len(result.Output) > 0 {
				if err := json.Unmarshal(result.Output, &m.data); err != nil {
					m.status = "Forecast response could not be parsed: " + err.Error()
				} else {
					m.status = "Forecast ready · " + m.data.Retrieved
				}
			} else if err := json.Unmarshal(msg.data, &m.data); err != nil {
				m.status = "Forecast response is empty or invalid"
			} else {
				m.status = "Forecast ready · " + m.data.Retrieved
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
		case "settings.update":
			m.settings = msg.data
			m.decodeSettings(msg.data)
			m.status = "Settings saved locally"
		case "auth.status":
			m.login = msg.data
			var state struct {
				Authenticated bool   `json:"authenticated"`
				Message       string `json:"message"`
				Login         struct {
					Status          string `json:"status"`
					RequestID       string `json:"request_id"`
					VerificationURL string `json:"verification_url"`
					Instructions    string `json:"instructions"`
				} `json:"login"`
			}
			_ = json.Unmarshal(msg.data, &state)
			if state.Login.RequestID != "" {
				m.loginRequestID = state.Login.RequestID
			}
			m.loginMessage = strings.TrimSpace(strings.Join([]string{state.Message, state.Login.Instructions, state.Login.VerificationURL}, "\n"))
			m.loginURL = state.Login.VerificationURL
			if state.Authenticated || state.Login.Status == "complete" {
				m.loginFlow = false
				m.loginMessage = "Application Default Credentials are ready."
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
			if m.loginFlow {
				return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return loginPollMsg{} })
			}
		case "locations.list":
			var result struct {
				Locations []Location `json:"locations"`
			}
			if err := json.Unmarshal(msg.data, &result); err == nil {
				if len(result.Locations) == 0 && !m.autoImported && len(m.data.Locations) > 0 {
					m.autoImported = true
					m.importQueue = append([]Location(nil), m.data.Locations...)
					return m, m.saveNextImport()
				}
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
				m.data.Locations = result.Locations
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
				if len(m.importQueue) > 0 {
					m.importQueue = m.importQueue[1:]
					if len(m.importQueue) > 0 {
						return m, m.saveNextImport()
					}
					return m, m.run("locations.list", nil)
				}
				m.data.Locations = append(m.data.Locations, result.Location)
				m.selected = len(m.data.Locations) - 1
				m.status = "Saved " + result.Location.Name
				m.screen = ScreenForecast
				return m, m.run("locations.list", nil)
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
				m.screen = ScreenForecast
				return m, nil
			}
			if key == "tab" {
				m.settingIndex = (m.settingIndex + 1) % (len(settingRows()) + 1)
				m.settingsEdit = ""
				return m, nil
			}
			if key == "backspace" && len(m.settingsEdit) > 0 {
				m.settingsEdit = m.settingsEdit[:len(m.settingsEdit)-1]
				return m, nil
			}
			if m.settingIndex == 4 && (key == "c" || key == "f") {
				m.settingsValues["units"] = strings.ToUpper(key)
				return m, m.saveSettings()
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
			if m.screen == ScreenForecast {
				m.move(-1)
			}
		case "down", "j":
			if m.screen == ScreenForecast {
				m.move(1)
			}
		case "left", "h":
			if m.screen == ScreenForecast {
				m.focusSidebar = true
			} else if m.screen == ScreenSearch && len(m.candidates) > 0 {
				m.candidate = clamp(m.candidate-1, 0, len(m.candidates)-1)
			}
		case "right":
			if m.screen == ScreenForecast {
				m.focusSidebar = false
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
		case "/":
			m.screen = ScreenSearch
			m.search = ""
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

func (m *Model) move(delta int) {
	if m.focusSidebar {
		if len(m.data.Locations) > 0 {
			m.selected = clamp(m.selected+delta, 0, len(m.data.Locations)-1)
		}
		return
	}
	if m.tab != TabDaily {
		m.hour = clamp(m.hour+delta, 0, max(0, m.hourCount()-1))
	}
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

func (m Model) View() tea.View {
	var body string
	switch m.screen {
	case ScreenSearch:
		body = m.searchView()
	case ScreenSettings:
		body = m.settingsView()
	case ScreenLogin:
		body = m.loginView()
	case ScreenSpending:
		body = m.spendingView()
	case ScreenHelp:
		body = helpView()
	default:
		body = m.forecastView()
	}
	if m.useASCII {
		body = asciiFallback(body)
	}
	if m.noColor {
		return tea.NewView(body)
	}
	return tea.NewView(lipgloss.NewStyle().Foreground(lipgloss.Color("#DDEAF7")).Render(body))
}

func (m Model) forecastView() string {
	if m.width < 1 {
		m.width = 80
	}
	if m.height < 1 {
		m.height = 24
	}
	brand := "WEATHERNEXT 3  /  ENSEMBLE FORECAST"
	if m.useASCII {
		brand = "WEATHERNEXT 3  /  ENSEMBLE FORECAST"
	}
	header := brand + "\n"
	if m.selectedLocation() != nil {
		l := m.selectedLocation()
		header += fmt.Sprintf("%s   %.4f, %.4f   run %s\n", l.Name, l.Latitude, l.Longitude, initTime(m.currentProduct()))
	} else {
		header += "No saved forecast loaded. Press ? for help.\n"
	}
	header += "[1] Hourly   [2] Daily   [3] Details   / Search   r Refresh   b Spend   s Settings   l Login\n"
	var content string
	switch m.tab {
	case TabHourly:
		content = m.hourlyView()
	case TabDaily:
		content = m.dailyView()
	case TabDetails:
		content = m.detailsView()
	}
	footer := m.status
	if m.loading {
		footer = "Working: " + m.busy + "  ·  Esc requests cancellation"
	}
	if footer == "" {
		footer = "Billed bytes are read from BigQuery job output; estimates and pending jobs are shown separately."
	}
	if m.width < 72 {
		return header + "\n" + content + "\n" + footer + "\nq quit · ? help"
	}
	left := m.sidebar()
	available := m.width - 30
	if available < 36 {
		return header + "\n" + content + "\n" + footer + "\nq quit · ? help"
	}
	divider, nav := "| ", "h locations · arrows forecast · Tab view · q quit · ? help"
	if !m.useASCII {
		divider, nav = "│ ", "←/h locations · ↑↓/jk forecast · Tab view · q quit · ? help"
	}
	return header + lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(25).Render(left), divider, content) + "\n" + footer + "\n" + nav
}

func (m Model) sidebar() string {
	var b strings.Builder
	b.WriteString("LOCATIONS\n")
	for i, l := range m.data.Locations {
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		name := l.Name
		if name == "" {
			name = fmt.Sprintf("%.3f, %.3f", l.Latitude, l.Longitude)
		}
		b.WriteString(marker + truncate(name, 21) + "\n")
	}
	if len(m.data.Locations) == 0 {
		b.WriteString("No saved locations\n")
	}
	return b.String()
}

func (m Model) hourlyView() string {
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No cached forecast. Press r to query after Google Cloud setup."
	}
	limit := min(len(p.Forecast), max(1, m.height-11))
	start := clamp(m.hour-limit/2, 0, max(0, len(p.Forecast)-limit))
	var b strings.Builder
	if m.useASCII {
		b.WriteString("TIME          TEMP C (P10-P90)     RAIN mm   CLOUD   WIND km/h\n")
	} else {
		b.WriteString("TIME          TEMP °C (P10–P90)    RAIN mm   CLOUD   WIND km/h\n")
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
		b.WriteString(fmt.Sprintf("%s %-12s %5.1f (%4.1f–%4.1f) %7.2f   %3.0f%%    %5.1f\n", cursor, t, temp, p10, p90, rain, cloud, wind))
	}
	b.WriteString("\nPercentile ranges describe ensemble spread; they are not a calibrated confidence score.\n")
	return b.String()
}

type daySummary struct {
	date                            string
	count                           int
	tempSum, minTemp, maxTemp, rain float64
	rainCount                       int
}

func (m Model) dailyView() string {
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No cached forecast. Press r to query after Google Cloud setup."
	}
	days := map[string]*daySummary{}
	for _, row := range p.Forecast {
		ts, err := time.Parse(time.RFC3339, str(row["time"]))
		if err != nil {
			continue
		}
		d := ts.Local().Format("2006-01-02")
		s := days[d]
		temp := tempValue(number(row["temperature_2m_mean"]), m.unit())
		if s == nil {
			s = &daySummary{date: d, minTemp: temp, maxTemp: temp}
			days[d] = s
		}
		s.count++
		s.tempSum += temp
		if temp < s.minTemp {
			s.minTemp = temp
		}
		if temp > s.maxTemp {
			s.maxTemp = temp
		}
		if v, ok := row["total_precipitation_1hr_mean"]; ok && v != nil {
			s.rain += number(v) * 1000
			s.rainCount++
		}
	}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("DAY          MEAN TEMP   HOURLY-MEAN LOW/HIGH   MEAN RAIN TOTAL   HOURS\n")
	for _, k := range keys {
		d := days[k]
		coverage := "partial"
		if d.count >= 23 {
			coverage = "complete"
		}
		unit := m.unit()
		b.WriteString(fmt.Sprintf("%-12s %5.1f °%s    %5.1f / %5.1f °%s       %6.2f mm      %3d (%s)\n", k, d.tempSum/float64(d.count), unit, d.minTemp, d.maxTemp, unit, d.rain, d.count, coverage))
	}
	b.WriteString("\nRain is the sum of hourly mean precipitation. Daily low/high use hourly means, not ensemble-member daily extrema.\n")
	return b.String()
}

func (m Model) detailsView() string {
	p := m.currentProduct()
	if p == nil || len(p.Forecast) == 0 {
		return "No forecast details available."
	}
	i := clamp(m.hour, 0, len(p.Forecast)-1)
	row := p.Forecast[i]
	keys := make([]string, 0, len(row))
	for k := range row {
		if k != "time" && k != "hours" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("HOURLY MODEL FIELDS · " + localTime(str(row["time"])) + "\n")
	for _, k := range keys {
		v := row[k]
		if v == nil {
			continue
		}
		b.WriteString(fmt.Sprintf("%-54s %s %s\n", k, displayField(k, v, m.unit()), fieldUnit(k, m.unit())))
	}
	b.WriteString("\nUse ↑/↓ to inspect another hour. Null fields are omitted.\n")
	return b.String()
}

func (m Model) spendingView() string {
	var b strings.Builder
	b.WriteString("SPENDING & BIGQUERY JOBS\n\n")
	if len(m.spending) > 0 {
		b.WriteString(prettyJSON(m.spending) + "\n")
	} else {
		b.WriteString("No spending summary loaded.\n")
	}
	if len(m.jobs) > 0 {
		b.WriteString("\nRECENT JOBS\n" + prettyJSON(m.jobs))
	}
	b.WriteString("\n\nBilled bytes come from the current script's BigQuery job metadata. Cost is an estimate based on configured rates; cache hits bill zero.\n")
	return b.String()
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
	if m.mapsKey == "" {
		b.WriteString("Optional session key: set GOOGLE_MAPS_API_KEY in the environment.\n")
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
	b.WriteString(fmt.Sprintf("%s%-60s %s\n", mark, "Maps API key (session only, never saved)", keyText))
	b.WriteString("\nTab selects · type value · Enter saves · c/f sets temperature unit · Esc returns\n")
	b.WriteString("Daily/monthly byte thresholds stop the app before a later job after reported billed bytes reach the threshold; they do not cap active queries or other Google Cloud usage.\n")
	b.WriteString("Set GOOGLE_MAPS_API_KEY in the environment to keep the key outside settings.\n")
	return b.String()
}

func settingRows() []struct{ key, label string } {
	return []struct{ key, label string }{
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

func (m Model) loginView() string {
	var status map[string]any
	_ = json.Unmarshal(m.login, &status)
	b := strings.Builder{}
	b.WriteString("GOOGLE CLOUD LOGIN\n\n")
	if len(status) > 0 {
		b.WriteString(prettyJSON(m.login) + "\n\n")
	}
	if m.loginMessage != "" {
		b.WriteString(m.loginMessage + "\n\n")
	}
	if m.loginFlow {
		b.WriteString("Paste the one-time Google code here: " + strings.Repeat("*", len(m.loginCode)) + "_\nEnter submits · o opens verification URL · Esc cancels the login\n")
	} else {
		b.WriteString("Press a to start Google login. The app opens the verification URL shown above; finish sign-in there, then paste the code here.\n")
	}
	b.WriteString("Credentials are handled by gcloud Application Default Credentials. Codes are held in memory only.\n")
	return b.String()
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
	return "KEYS\n\n↑ / k       previous location or hour\n↓ / j       next location or hour\n← / h       select location sidebar\n→           return focus to forecast\nTab         next forecast tab\n1 / 2 / 3   hourly / daily / details\n/           search locations; Enter searches, arrows select candidates\nr           fetch selected location for configured hours (BigQuery may bill)\nb           billed-byte ledger and job history\ns           settings: units, hours, byte cap, estimate rates, session Maps key\nl / a      login status / start login; Esc cancels, code stays masked\nEsc         back or cancel current operation\nq           quit\n\nForecasts open from a local JSON file offline. Refresh sends a BigQuery request.\n"
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
	return displayNumber(n)
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
func (m *Model) saveNextImport() tea.Cmd {
	if len(m.importQueue) == 0 {
		return m.run("locations.list", nil)
	}
	l := m.importQueue[0]
	name := l.Name
	if name == "" {
		name = fmt.Sprintf("%.4f, %.4f", l.Latitude, l.Longitude)
	}
	return m.run("locations.save", map[string]any{"name": name, "latitude": l.Latitude, "longitude": l.Longitude})
}
func (m *Model) decodeSettings(raw json.RawMessage) {
	var result struct {
		Settings map[string]any `json:"settings"`
		Billing  map[string]any `json:"billing"`
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
			m.status = "Type a value, then press Enter to save"
			return nil
		}
		if strings.EqualFold(input, "off") {
			m.settingsValues[field] = nil
		} else if field == "default_hours" || isByteLimitField(field) {
			v, err := strconv.ParseInt(input, 10, 64)
			if err != nil || v <= 0 || field == "default_hours" && v > 360 {
				m.status = "Enter a positive integer (hours: 1–360) or 'off' to unset a byte threshold"
				return nil
			}
			m.settingsValues[field] = v
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
	for _, k := range []string{"default_hours", "max_bytes_billed", "max_daily_billed_bytes", "max_monthly_billed_bytes", "units"} {
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
