package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// Print renders a compact, pipe-friendly wttr.in-style forecast summary.
func Print(raw []byte, out io.Writer) error {
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("read forecast JSON: %w", err)
	}
	if len(d.Locations) == 0 {
		return fmt.Errorf("forecast JSON contains no locations")
	}
	for i, l := range d.Locations {
		if i > 0 {
			fmt.Fprintln(out)
		}
		name := l.Name
		if name == "" {
			name = fmt.Sprintf("%.4f, %.4f", l.Latitude, l.Longitude)
		}
		fmt.Fprintf(out, "WeatherNext 3 · %s\n", name)
		fmt.Fprintf(out, "Coordinates: %.5f, %.5f · retrieved %s · init %s\n", l.Latitude, l.Longitude, localTime(d.Retrieved), initTime(productFor(&l)))
		p := productFor(&l)
		if p == nil || len(p.Forecast) == 0 {
			fmt.Fprintln(out, "No forecast rows.")
			continue
		}
		days := make(map[string]*daySummary)
		fmt.Fprintln(out, "\nHourly forecast")
		fmt.Fprintln(out, "Time                 Temp °C (p10–p90)     Rain mm  Cloud  Wind km/h")
		for _, row := range p.Forecast {
			t, err := time.Parse(time.RFC3339, str(row["time"]))
			if err != nil {
				continue
			}
			fmt.Fprintf(out, "%-20s %5.1f (%4.1f–%4.1f)       %5.2f   %3.0f%%    %5.1f\n", t.Local().Format("Mon 02 Jan 15:04"), tempValue(number(row["temperature_2m_mean"]), "C"), tempValue(number(row["temperature_2m_p10"]), "C"), tempValue(number(row["temperature_2m_p90"]), "C"), number(row["total_precipitation_1hr_mean"])*1000, number(row["total_cloud_cover_mean"])*100, number(row["wind_speed_10m_mean"])*3.6)
			d := t.Local().Format("2006-01-02")
			temp := tempValue(number(row["temperature_2m_mean"]), "C")
			s := days[d]
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
		fmt.Fprintln(out, "\nDaily summary · rain sums hourly means; temperature extrema are extrema of hourly means")
		for _, k := range keys {
			s := days[k]
			coverage := "partial"
			if s.count >= 23 {
				coverage = "complete"
			}
			fmt.Fprintf(out, "%s  %5.1f °C mean  %5.1f/%5.1f °C hourly-mean low/high  %5.2f mm rain  %d hours (%s)\n", k, s.tempSum/float64(s.count), s.minTemp, s.maxTemp, s.rain, s.count, coverage)
		}
		fmt.Fprintln(out, "\nEnsemble percentile spread (p10–p90) is not a calibrated confidence score.")
	}
	return nil
}

func productFor(l *Location) *Product {
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
