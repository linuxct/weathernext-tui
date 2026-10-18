package tui

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Prefer available hourly medians for the whole group. Use hourly ensemble
// means only if this variable has no usable medians; do not mix statistics.
func centralSamples(rows []map[string]any, base string) ([]float64, string) {
	for _, source := range []struct{ suffix, label string }{{"_p50", "median"}, {"_mean", "mean"}, {"", "value"}} {
		values := make([]float64, 0, len(rows))
		for _, row := range rows {
			n := number(row[base+source.suffix])
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				values = append(values, n)
			}
		}
		if len(values) > 0 {
			return values, source.label
		}
	}
	return nil, "unavailable"
}

func centralAggregate(rows []map[string]any, base string) (float64, int, string) {
	values, source := centralSamples(rows, base)
	if len(values) == 0 {
		return math.NaN(), 0, source
	}
	sum := 0.0
	for _, n := range values {
		sum += n
	}
	return sum / float64(len(values)), len(values), source
}

func weatherFieldBase(key string) string {
	if strings.HasSuffix(key, "_mean") {
		return strings.TrimSuffix(key, "_mean")
	}
	if pos := strings.LastIndex(key, "_p"); pos >= 0 {
		if _, err := strconv.Atoi(key[pos+2:]); err == nil {
			return key[:pos]
		}
	}
	return key
}

func readableAggregateLines(p *Product, width int, unit string, daily bool) []string {
	type group struct {
		label string
		rows  []map[string]any
	}
	groups := []*group{}
	byKey := map[string]*group{}
	for _, row := range p.Forecast {
		ts, err := time.Parse(time.RFC3339, str(row["time"]))
		if err != nil {
			continue
		}
		local := ts.Local()
		key, label := local.Format("2006-01-02"), local.Format("Mon 02 Jan")
		if !daily {
			part := daypartIndex(local.Hour())
			key += fmt.Sprintf("/%d", part)
			label += " · " + titleCase(daypartName(part))
		}
		g := byKey[key]
		if g == nil {
			g = &group{label: label}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, row)
	}
	lines := []string{}
	for _, g := range groups {
		header := fmt.Sprintf("%s · %d hourly records", g.label, len(g.rows))
		lines = append(lines, strings.Split(lipgloss.Wrap(header, max(1, width), " "), "\n")...)
		variables := map[string]bool{}
		for _, row := range g.rows {
			for key := range row {
				if key != "time" && key != "hours" {
					variables[weatherFieldBase(key)] = true
				}
			}
		}
		keys := make([]string, 0, len(variables))
		for key := range variables {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		priority := map[string]int{"temperature_2m": 1, "total_precipitation_1hr": 2,
			"wind_speed_10m": 3, "total_cloud_cover": 4, "dewpoint_temperature_2m": 5,
			"mean_sea_level_pressure": 6}
		sort.SliceStable(keys, func(i, j int) bool {
			left, right := priority[keys[i]], priority[keys[j]]
			if left == 0 {
				left = 100
			}
			if right == 0 {
				right = 100
			}
			return left < right
		})
		for _, base := range keys {
			value, count, source := centralAggregate(g.rows, base)
			shown := displayField(base, value, unit)
			label := humanFieldName(base)
			units := fieldUnit(base, unit)
			if units == "" {
				units = "model units"
			}
			// Accumulated hourly precipitation is averaged here, whereas the cards
			// sum hourly central amounts over the displayed period/day.
			if units == "mm" {
				units = "mm/hour (average)"
			}
			line := "  " + label + ": " + shown
			if count > 0 && units != "" {
				line += " " + units
			}
			if source == "mean" {
				line += " · mean fallback"
			}
			if count < len(g.rows) {
				line += fmt.Sprintf(" · %d/%dh", count, len(g.rows))
			}
			lines = append(lines, strings.Split(lipgloss.Wrap(line, max(1, width), " "), "\n")...)
		}
	}
	return lines
}
