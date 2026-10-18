package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"weathernext/internal/protocol"
)

const sample = `{"retrieved_at_utc":"2026-10-18T08:00:00Z","dataset":"weathernext_3","requested_hours":24,"locations":[{"id":"example-place","name":"Example place","latitude":12.3456,"longitude":45.6789,"products":{"surface":{"init_time_utc":"2026-10-17T18:00:00Z","forecast":[{"time":"2026-10-18T09:00:00Z","temperature_2m_mean":292.35,"temperature_2m_p10":291.65,"temperature_2m_p90":292.95,"total_precipitation_1hr_mean":0.0004,"total_cloud_cover_mean":0.8,"wind_speed_10m_mean":3.1389}]}}}]}`

func TestNavigationAndResize(t *testing.T) {
	m := New([]byte(sample), nil)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
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
