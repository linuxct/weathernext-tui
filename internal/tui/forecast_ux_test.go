package tui

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func longForecastModel() *Model {
	m := New([]byte(sample), nil)
	m.screen = ScreenForecast
	m.onboarding = false
	m.noColor = true
	p := m.data.Locations[0].Products["surface"]
	p.Forecast = nil
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
	for hour := 0; hour < 360; hour++ {
		p.Forecast = append(p.Forecast, map[string]any{
			"time":               start.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339),
			"temperature_2m_p10": 273.15, "temperature_2m_p50": 293.15, "temperature_2m_p90": 313.15, "temperature_2m_mean": 303.15,
			"total_precipitation_1hr_p50": 0.0001, "total_precipitation_1hr_mean": 0.002,
			"total_cloud_cover_p50": 0.2, "wind_speed_10m_p50": 3.0,
		})
	}
	m.data.Locations[0].Products["surface"] = p
	return &m
}

func TestEmptyLocationListNeverReimportsCachedLocations(t *testing.T) {
	m := New([]byte(sample), nil)
	_, cmd := m.Update(backendResult{method: "locations.list", data: json.RawMessage(`{"locations":[]}`)})
	if cmd != nil || len(m.data.Locations) != 0 {
		t.Fatal("an empty saved list resurrected a cached location")
	}
}

func TestCentralEstimatesPreserveRawDataAndUseMeanFallback(t *testing.T) {
	m := longForecastModel()
	m.width, m.height = 140, 80
	p := m.currentProduct()
	row := p.Forecast[0]
	row["dewpoint_temperature_2m_p50"] = nil
	row["dewpoint_temperature_2m_mean"] = 283.15
	row["dewpoint_temperature_2m_p10"] = 280.0
	before, _ := json.Marshal(row)
	lines := strings.Join(readableForecastLines(&Product{Forecast: []map[string]any{row}}, 100, "C", false), "\n")
	for _, want := range []string{"Temperature 2 m: 20.0", "Dewpoint Temperature 2 m: 10.0", "Wind Speed 10 m: 10.8"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in %s", want, lines)
		}
	}
	for _, unwanted := range []string{"P10", "P90", "P25", "P75", "Temperature 2 m Mean: 30.0"} {
		if strings.Contains(lines, unwanted) {
			t.Fatalf("unexpected statistic %q in %s", unwanted, lines)
		}
	}
	if got := forecastPeriods(p)[0].temp; math.Abs(got-20) > 1e-9 {
		t.Fatalf("period temperature=%v; expected median-derived 20 C", got)
	}
	for _, daily := range []bool{false, true} {
		view := m.summaryView(108)
		if daily {
			view = m.dailyView(108)
		}
		if strings.Contains(view, "P10") || strings.Contains(view, "P90") || !strings.Contains(view, "+20°C") {
			t.Fatalf("default view contains wrong estimate:\n%s", view)
		}
	}
	m.settingsValues = map[string]any{"units": "F"}
	if view := m.dailyView(108); !strings.Contains(view, "+68°F") || !strings.Contains(view, "68/68°") {
		t.Fatalf("incorrect daily Fahrenheit conversion:\n%s", view)
	}
	after, _ := json.Marshal(row)
	if string(before) != string(after) {
		t.Fatal("presentation modified source data")
	}
	raw := strings.Join(rawJSONLines(row, 100), "\n")
	for _, key := range []string{"temperature_2m_p10", "temperature_2m_p50", "temperature_2m_p90", "temperature_2m_mean"} {
		if !strings.Contains(raw, key) {
			t.Fatalf("Raw lost %s", key)
		}
	}
}

func selectedCardColumn(view string) int {
	for _, line := range strings.Split(view, "\n") {
		if i := strings.Index(line, "> "); i >= 0 {
			return lipgloss.Width(line[:i])
		}
	}
	return -1
}

func TestSelectionMovesAcrossGridAndPaginatesBothTabs(t *testing.T) {
	for _, tab := range []Tab{TabHourly, TabDaily} {
		m := longForecastModel()
		m.width, m.height = 140, 80
		m.tab = tab
		width, _ := m.forecastBodySize()
		first := m.forecastContent(width)
		initial := m.forecastCardsLayout(width, tab == TabDaily)
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		next := m.forecastContent(width)
		if selectedCardColumn(next) <= selectedCardColumn(first) {
			t.Fatalf("tab %v selection remained in same column", tab)
		}
		if m.forecastCardsLayout(width, tab == TabDaily).Start != initial.Start {
			t.Fatal("page scrolled before selection reached its edge")
		}
		firstTime := str(m.readableProduct(tab == TabDaily).Forecast[0]["time"])
		if firstTime == str(m.currentProduct().Forecast[0]["time"]) {
			t.Fatal("readable data did not follow selected period/day")
		}
		for i := 1; i < initial.End; i++ {
			m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyRight}))
		}
		page := m.forecastCardsLayout(width, tab == TabDaily)
		if page.Start != initial.End {
			t.Fatalf("tab %v failed to advance to next card page: %+v", tab, page)
		}
		m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft}))
		if m.forecastCardsLayout(width, tab == TabDaily).Start != 0 {
			t.Fatal("could not return to preceding page")
		}
	}
}

func TestCardRowsCappedAndReadablePanelFitsViewport(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {140, 60}, {140, 80}, {160, 100}, {80, 40}, {80, 24}} {
		for _, tab := range []Tab{TabHourly, TabDaily} {
			t.Run(fmt.Sprintf("%dx%d/tab%d", size[0], size[1], tab), func(t *testing.T) {
				m := longForecastModel()
				m.width, m.height = size[0], size[1]
				m.tab = tab
				width, height := m.forecastBodySize()
				layout := m.forecastCardsLayout(width, tab == TabDaily)
				if layout.End-layout.Start > layout.Columns*3 {
					t.Fatalf("more than three rows: %+v", layout)
				}
				view := m.forecastContent(width)
				if lipgloss.Height(view) > height {
					t.Fatalf("forecast height %d exceeds available %d:\n%s", lipgloss.Height(view), height, view)
				}
				if strings.Contains(view, "more below") {
					t.Fatalf("card content was clipped:\n%s", view)
				}
				if m.height >= 60 && m.readableVisibleLines() <= 5 {
					t.Fatalf("readable card did not grow: %d lines", m.readableVisibleLines())
				}
				for _, line := range strings.Split(view, "\n") {
					if lipgloss.Width(line) > width {
						t.Fatalf("line wider than viewport: %s", line)
					}
				}
			})
		}
	}
}

func TestDailyAndDaypartReadableDataAggregateAvailableMedians(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
	p := &Product{}
	for h := 0; h < 24; h++ {
		p.Forecast = append(p.Forecast, map[string]any{
			"time":               start.Add(time.Duration(h) * time.Hour).Format(time.RFC3339),
			"temperature_2m_p50": 273.15 + float64(h), "temperature_2m_mean": 500.0,
			"temperature_2m_p10": 200.0, "temperature_2m_p90": 600.0,
			"total_precipitation_1hr_p50": 0.001, "total_precipitation_1hr_mean": 0.500,
			"dewpoint_temperature_2m_mean": 273.15,
		})
	}
	// Missing hourly medians do not get replaced with extreme means mid-group.
	p.Forecast[0]["total_precipitation_1hr_p50"] = nil
	daily := strings.Join(readableForecastLines(p, 120, "C", true), "\n")
	for _, want := range []string{"Temperature 2 m: 11.5 °C", "Total Precipitation 1 hr: 1.00 mm/hour (average) · 23/24h", "Dewpoint Temperature 2 m: 0.0 °C · mean fallback"} {
		if !strings.Contains(daily, want) {
			t.Fatalf("daily aggregate missing %q:\n%s", want, daily)
		}
	}
	if strings.Count(daily, "Temperature 2 m:") != 2 {
		t.Fatalf("daily fields repeated hourly:\n%s", daily)
	}
	periods := strings.Join(readableForecastLines(p, 120, "C", false), "\n")
	for _, want := range []string{"Night", "Morning", "Noon", "Evening", "Temperature 2 m: 2.5 °C", "Temperature 2 m: 8.5 °C", "Temperature 2 m: 14.5 °C", "Temperature 2 m: 20.5 °C"} {
		if !strings.Contains(periods, want) {
			t.Fatalf("period aggregate missing %q:\n%s", want, periods)
		}
	}
	if strings.Count(periods, "Temperature 2 m:") != 8 {
		t.Fatalf("period fields repeated hourly:\n%s", periods)
	}
	if strings.Contains(daily, "00:00") || strings.Contains(periods, "P90") {
		t.Fatal("hourly rows or bounds leaked into aggregate view")
	}
}

func TestRefreshAndLocationSwitchKeepWorkspaceConsistent(t *testing.T) {
	m := longForecastModel()
	second := m.data.Locations[0]
	second.ID, second.Name, second.Latitude = "second", "Second", 40
	m.data.Locations = append(m.data.Locations, second)
	m.selected, m.hour, m.readableScroll = 1, 200, 5
	incoming := second
	incoming.Products = map[string]Product{"surface": {Forecast: []map[string]any{{"time": "2026-10-18T00:00:00Z", "temperature_2m_p50": 293.15}}}}
	raw, _ := json.Marshal(map[string]any{"output": Data{Locations: []Location{incoming}}})
	m.Update(backendResult{method: "forecast.fetch", data: raw})
	if len(m.data.Locations) != 2 || m.selected != 1 || m.hour != 0 || m.readableScroll != 0 {
		t.Fatal("refresh lost saved locations or left an invalid selection")
	}
	if len(m.data.Locations[0].Products["surface"].Forecast) != 360 {
		t.Fatal("refresh discarded another place's cached forecast")
	}
	m.focusSidebar = true
	m.hour, m.readableScroll, m.detailScroll = 10, 4, 4
	m.move(-1)
	if m.selected != 0 || m.hour != 0 || m.readableScroll != 0 || m.detailScroll != 0 {
		t.Fatal("switching location retained stale forecast cursors")
	}
}
