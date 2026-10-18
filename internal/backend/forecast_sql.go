package backend

import (
	"fmt"
	"strings"
)

func discoverySQL(table, cycle string) string {
	cycleFilter := ""
	if cycle == "main" {
		cycleFilter = "AND EXTRACT(HOUR FROM t.init_time) IN (0, 6, 12, 18)"
	}
	return fmt.Sprintf(`SELECT FORMAT_TIMESTAMP('%%Y-%%m-%%dT%%H:%%M:%%E6SZ', MAX(t.init_time), 'UTC') AS init_time
FROM %s AS t
WHERE t.init_time >= @since AND t.init_time <= @now
  %s`, quoteTable(table), cycleFilter)
}

func forecastSQL(table string, pointIDs []int) string {
	selections := make([]string, 0, len(pointIDs))
	for _, index := range pointIDs {
		point := fmt.Sprintf("ST_GEOGPOINT(@longitude_%d, @latitude_%d)", index, index)
		selections = append(selections, fmt.Sprintf(`(
SELECT %d AS id, t.init_time,
       ST_ASGEOJSON(t.geography) AS grid_center,
       ST_ASGEOJSON(t.geography_polygon) AS grid_polygon,
       ST_DISTANCE(t.geography, %s) AS distance_m,
       TO_JSON_STRING(ARRAY(
         SELECT AS STRUCT f.* FROM UNNEST(t.forecast) AS f
         WHERE f.time > @window_start AND f.time <= @window_end
           AND (NOT @future_only OR f.time >= @now)
         ORDER BY f.time
       )) AS forecast_json
FROM %s AS t
WHERE t.init_time = @init_time
  AND ST_DWITHIN(t.geography, %s, 10000)
  AND ST_INTERSECTS(t.geography_polygon, %s)
ORDER BY distance_m, ST_ASTEXT(t.geography)
LIMIT 1
)`, index, point, quoteTable(table), point, point))
	}
	return strings.Join(selections, "\nUNION ALL\n") + "\nORDER BY id\n"
}

func quoteTable(table string) string { return "`" + strings.ReplaceAll(table, "`", "") + "`" }
