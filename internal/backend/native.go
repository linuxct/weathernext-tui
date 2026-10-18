package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"weathernext/internal/protocol"
)

// NativeClient implements the TUI backend directly in Go. Its database schema
// intentionally matches the former Python backend so existing local state is kept.
type NativeClient struct {
	home    string
	store   *store
	auth    *authService
	seq     atomic.Uint64
	mu      sync.Mutex
	queryMu sync.Mutex
}

func StartNative(home string) (*NativeClient, error) {
	if home == "" {
		var err error
		home, err = FindHome()
		if err != nil {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("WeatherNext folder %q is not an accessible directory", absolute)
	}
	data, err := openStore(absolute)
	if err != nil {
		return nil, err
	}
	return &NativeClient{home: absolute, store: data, auth: newAuthService(absolute)}, nil
}

func FindHome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("WEATHERNEXT_HOME")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err == nil {
			if info, statErr := os.Stat(absolute); statErr == nil && info.IsDir() {
				return absolute, nil
			}
		}
		return "", fmt.Errorf("WEATHERNEXT_HOME=%q is not an accessible directory", configured)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find current directory: %w", err)
	}
	return filepath.Abs(cwd)
}

func (c *NativeClient) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.close()
}

func (c *NativeClient) Call(ctx context.Context, method string, params map[string]any, onEvent func(protocol.Message)) (json.RawMessage, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("native WeatherNext backend is unavailable")
	}
	if params == nil {
		params = map[string]any{}
	}
	data, err := c.dispatch(ctx, method, params, onEvent)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode %s response: %w", method, err)
	}
	return encoded, nil
}

func (c *NativeClient) dispatch(ctx context.Context, method string, params map[string]any, onEvent func(protocol.Message)) (any, error) {
	switch method {
	case "settings.get":
		return map[string]any{"settings": sanitizeSettings(c.store.getSettingsMap("app")), "billing": sanitizeSettings(c.store.getSettingsMap("billing"))}, nil
	case "settings.update":
		return c.updateSettings(params)
	case "auth.status":
		return c.auth.status(ctx), nil
	case "auth.start":
		return c.auth.start(), nil
	case "auth.respond":
		return c.auth.respond(params), nil
	case "auth.check":
		return c.checkTarget(ctx, params)
	case "maps.setup":
		return c.setupMaps(ctx), nil
	case "locations.list":
		locations, err := c.store.listLocations()
		return map[string]any{"locations": locations}, err
	case "locations.search":
		return c.searchLocations(ctx, params)
	case "locations.resolve":
		return c.resolveLocation(ctx, params)
	case "locations.save":
		return c.saveLocation(params)
	case "locations.delete":
		id := stringValue(params["id"])
		if id == "" {
			return nil, errors.New("select a saved location to delete")
		}
		deleted, err := c.store.deleteLocation(id)
		return map[string]any{"deleted": deleted, "id": id}, err
	case "forecast.open":
		return c.openForecast(params)
	case "forecast.fetch":
		return c.fetchForecast(ctx, params, onEvent)
	case "spending.summary":
		return c.store.spendingSummary()
	case "spending.jobs":
		limit := intValueOrZero(params["limit"])
		if limit <= 0 {
			limit = 500
		}
		if limit > 2000 {
			limit = 2000
		}
		jobs, err := c.store.jobs(int(limit))
		return map[string]any{"jobs": jobs}, err
	default:
		return nil, fmt.Errorf("unsupported backend operation %q", method)
	}
}

var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
var datasetPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
var tablePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c *NativeClient) target(params map[string]any) (project, dataset, table string, err error) {
	settings := c.store.getSettingsMap("app")
	project = firstString(strings.TrimSpace(stringValue(params["project"])), strings.TrimSpace(stringValue(settings["target_project"])))
	dataset = firstString(strings.TrimSpace(stringValue(params["dataset"])), strings.TrimSpace(stringValue(settings["target_dataset"])))
	table = firstString(strings.TrimSpace(stringValue(params["table"])), strings.TrimSpace(stringValue(settings["target_table"])))
	if project == "" || dataset == "" || table == "" {
		return project, dataset, table, errors.New("Enter the Google Cloud project ID, WeatherNext dataset, and table in Settings.")
	}
	if !projectIDPattern.MatchString(project) || len(dataset) > 1024 || !datasetPattern.MatchString(dataset) || len(table) > 1024 || !tablePattern.MatchString(table) {
		return project, dataset, table, errors.New("The configured project, dataset, or table name is invalid. Review Settings.")
	}
	return project, dataset, table, nil
}

func (c *NativeClient) checkTarget(ctx context.Context, params map[string]any) (map[string]any, error) {
	result := map[string]any{"connected": false, "requires_login": false, "requires_config": false, "login_available": oauthClientID() != "" && oauthClientSecret() != ""}
	project, dataset, table, targetErr := c.target(params)
	if targetErr != nil {
		if strings.Contains(targetErr.Error(), "Enter the Google Cloud") {
			result["requires_config"] = true
		}
		result["message"] = targetErr.Error()
		return result, nil
	}
	result["target_project"], result["table"] = project, project+"."+dataset+"."+table
	client, err := newGoogleAPI(ctx, c.auth)
	if err != nil {
		result["requires_login"] = true
		result["message"] = "Sign in with the Google account that has access to this WeatherNext table. " + err.Error()
		return result, nil
	}
	metadata, err := client.getTable(ctx, project, dataset, table)
	if err != nil {
		message := err.Error()
		if strings.Contains(message, "NOT_FOUND") || strings.Contains(message, "notFound") || strings.Contains(message, "404") {
			result["requires_config"] = true
			result["message"] = "That table was not found for this Google account. Check the project ID, dataset, and table in Settings."
		} else if strings.Contains(message, "UNAUTHENTICATED") || strings.Contains(message, "invalid_grant") || strings.Contains(message, "401") || strings.Contains(message, "oauth2:") || strings.Contains(message, "token expired") {
			result["requires_login"] = true
			result["message"] = "Google sign-in has expired. Sign in again, then retry."
		} else {
			result["requires_config"] = true
			result["message"] = "Google credentials are active, but this account could not access the target table. Check the IDs and BigQuery permissions."
		}
		return result, nil
	}
	if fields, ok := metadata["schema"].(map[string]any); ok {
		if nested, ok := fields["fields"].([]any); ok {
			found := false
			for _, raw := range nested {
				if field, ok := raw.(map[string]any); ok && stringValue(field["name"]) == "forecast" {
					found = true
					break
				}
			}
			if !found {
				result["requires_config"] = true
				result["message"] = "The selected table has no forecast field. Review the WeatherNext table name."
				return result, nil
			}
		}
	}
	result["connected"] = true
	result["message"] = "Google sign-in and WeatherNext table access are ready."
	return result, nil
}

func (c *NativeClient) updateSettings(params map[string]any) (map[string]any, error) {
	values, ok := params["settings"].(map[string]any)
	if !ok {
		values = map[string]any{}
	}
	values = sanitizeSettings(values)
	current := c.store.getSettingsMap("app")
	for key, pattern := range map[string]*regexp.Regexp{"target_project": projectIDPattern, "target_dataset": datasetPattern, "target_table": tablePattern} {
		if raw, ok := values[key]; ok {
			value, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be text", key)
			}
			value = strings.TrimSpace(value)
			if value != "" && (len(value) > 1024 || !pattern.MatchString(value)) {
				return nil, fmt.Errorf("enter a valid %s", strings.ReplaceAll(strings.TrimPrefix(key, "target_"), "_", " "))
			}
			values[key] = value
		}
	}
	if raw, ok := values["default_hours"]; ok {
		n, valid := intValue(raw)
		if !valid || n < 1 || n > 360 {
			return nil, errors.New("forecast hours must be from 1 to 360")
		}
		values["default_hours"] = n
	}
	for _, key := range []string{"max_bytes_billed", "max_daily_billed_bytes", "max_monthly_billed_bytes"} {
		if raw, ok := values[key]; ok && raw != nil {
			n, valid := intValue(raw)
			if !valid || n <= 0 {
				return nil, fmt.Errorf("%s must be a positive integer or unset", key)
			}
			values[key] = n
		}
	}
	for k, v := range values {
		current[k] = v
	}
	if err := c.store.updateSettings(map[string]any{"app": current}); err != nil {
		return nil, err
	}
	if billing, ok := params["billing"].(map[string]any); ok {
		billing = sanitizeSettings(billing)
		for _, key := range []string{"usd_per_tib", "maps_usd_per_1000_requests"} {
			if raw, ok := billing[key]; ok && raw != nil {
				value, valid := floatValueOK(raw)
				if !valid || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
					return nil, fmt.Errorf("%s must be a nonnegative number", key)
				}
			}
		}
		if err := c.store.updateSettings(map[string]any{"billing": billing}); err != nil {
			return nil, err
		}
	}
	return map[string]any{"settings": sanitizeSettings(c.store.getSettingsMap("app")), "billing": sanitizeSettings(c.store.getSettingsMap("billing"))}, nil
}

func (c *NativeClient) openForecast(params map[string]any) (map[string]any, error) {
	var paths []string
	if path := strings.TrimSpace(stringValue(params["path"])); path != "" {
		paths = append(paths, path)
	} else {
		settings := c.store.getSettingsMap("app")
		if path := stringValue(settings["output_path"]); path != "" {
			paths = append(paths, path)
		}
		paths = append(paths, filepath.Join(c.home, "output", "forecast.json"), filepath.Join(c.home, ".local", "weathernext", "forecast-latest.json"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var output map[string]any
		if err = json.Unmarshal(data, &output); err != nil {
			continue
		}
		c.importForecast(output)
		return map[string]any{"output": output, "path": path, "cache_status": "local"}, nil
	}
	return nil, errors.New("no saved forecast is available. Add a location and fetch a forecast, or choose an output JSON file")
}

func (c *NativeClient) importForecast(output map[string]any) {
	locations, ok := output["locations"].([]any)
	if !ok {
		return
	}
	if c.store.getSetting("locations_empty_by_choice", false) == true {
		return
	}
	dataset := stringValue(output["dataset"])
	project := "local"
	if strings.Contains(dataset, ".") {
		project = strings.SplitN(dataset, ".", 2)[0]
	}
	hours := int(intValueOrZero(output["requested_hours"]))
	if hours <= 0 {
		hours = 24
	}
	reference := firstString(stringValue(output["window_reference"]), "retrieval")
	for _, item := range locations {
		value, ok := item.(map[string]any)
		if !ok {
			continue
		}
		lat, latOK := floatValueOK(value["latitude"])
		lon, lonOK := floatValueOK(value["longitude"])
		name := strings.TrimSpace(stringValue(value["name"]))
		if !latOK || !lonOK || name == "" {
			continue
		}
		key := coordinateKey(lat, lon)
		var dismissed int
		if c.store.db.QueryRow("SELECT 1 FROM dismissed_locations WHERE coordinate_key=?", key).Scan(&dismissed) == nil {
			continue
		}
		var id string
		_ = c.store.db.QueryRow("SELECT id FROM locations WHERE ABS(latitude-?)<0.000001 AND ABS(longitude-?)<0.000001", lat, lon).Scan(&id)
		if id == "" {
			sum := sha256.Sum256([]byte(fmt.Sprintf("weathernext:%s:%.6f:%.6f", name, lat, lon)))
			id = hex.EncodeToString(sum[:16])
			_ = c.store.saveLocation(map[string]any{"id": id, "name": name, "latitude": lat, "longitude": lon})
		}
		value["id"], value["name"] = id, name
		products, _ := value["products"].(map[string]any)
		for product, raw := range products {
			payload, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			init := normalizeTimestamp(stringValue(payload["init_time_utc"]))
			center, _ := payload["grid_center"].(map[string]any)
			coords, _ := center["coordinates"].([]any)
			if len(coords) < 2 || init == "" {
				continue
			}
			gridLon, _ := floatValueOK(coords[0])
			gridLat, _ := floatValueOK(coords[1])
			cacheDataset := dataset
			meta, _ := output["products"].(map[string]any)
			if pmeta, ok := meta[product].(map[string]any); ok && stringValue(pmeta["table"]) != "" {
				cacheDataset = dataset + "::" + stringValue(pmeta["table"])
			}
			_ = c.store.putForecast(project, cacheDataset, product, init, gridCell(gridLat, gridLon), lat, lon, hours, reference, payload)
		}
	}
}

func gridCell(lat, lon float64) string { return fmt.Sprintf("%.6f,%.6f", lat, lon) }

func normalizeTimestamp(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999 UTC", "2006-01-02 15:04:05 UTC"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	return value
}
func expectedCell(lat, lon float64) string {
	return gridCell(math.Floor(lat/0.1+0.5)*0.1, math.Floor(lon/0.1+0.5)*0.1)
}

func (c *NativeClient) fetchForecast(ctx context.Context, params map[string]any, onEvent func(protocol.Message)) (map[string]any, error) {
	settings := c.store.getSettingsMap("app")
	hours := int(intValueOrZero(params["hours"]))
	if hours == 0 {
		hours = int(intValueOrZero(settings["default_hours"]))
	}
	if hours == 0 {
		hours = 24
	}
	if hours < 1 || hours > 360 {
		return nil, errors.New("forecast hours must be between 1 and 360")
	}
	var idsRaw []any
	switch values := params["location_ids"].(type) {
	case []any:
		idsRaw = values
	case []string:
		for _, value := range values {
			idsRaw = append(idsRaw, value)
		}
	}
	if len(idsRaw) == 0 {
		return nil, errors.New("choose at least one saved location")
	}
	ids := make([]string, 0, len(idsRaw))
	for _, raw := range idsRaw {
		if value, ok := raw.(string); ok {
			ids = append(ids, value)
		}
	}
	locations := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		location, err := c.store.location(id)
		if err != nil {
			return nil, fmt.Errorf("saved location %s is unavailable", id)
		}
		if _, ok := floatValueOK(location["latitude"]); !ok {
			return nil, fmt.Errorf("refresh coordinates for %s before fetching", location["name"])
		}
		locations = append(locations, location)
	}
	project, datasetID, tableName, err := c.target(nil)
	if err != nil {
		return nil, err
	}
	if boolValue(params["include_station"]) {
		return nil, errors.New("station forecasts are not configured; select surface only")
	}
	table := fmt.Sprintf("%s.%s.%s", project, datasetID, tableName)
	cacheDataset := fmt.Sprintf("%s.%s::%s", project, datasetID, table)
	location := strings.TrimSpace(stringValue(settings["bq_location"]))
	cycle := firstString(stringValue(settings["cycle"]), "main")
	now := time.Now().UTC()
	start := now
	end := now.Add(time.Duration(hours) * time.Hour)
	output := map[string]any{"retrieved_at_utc": now.Format(time.RFC3339Nano), "dataset": project + "." + datasetID, "cycle": cycle, "requested_hours": hours, "window_reference": "retrieval", "future_only": true, "units": "Original source units; no conversion applied. See field descriptions.", "products": map[string]any{}, "locations": make([]any, len(locations))}
	outLocations := output["locations"].([]any)
	for i, loc := range locations {
		outLocations[i] = map[string]any{"id": loc["id"], "name": loc["name"], "latitude": loc["latitude"], "longitude": loc["longitude"], "products": map[string]any{}}
	}
	opID := fmt.Sprintf("forecast-%d", c.seq.Add(1))
	_ = c.store.startOperation("forecast.fetch", opID)
	done := false
	defer func() {
		if !done {
			c.store.finishOperation(opID, "failed", nil)
		}
	}()
	if onEvent != nil {
		emitProtocolEvent(onEvent, "progress", map[string]any{"stage": "run_discovery", "product": "surface"})
	}
	initTime := c.store.getRun(project, cacheDataset, "surface", cycle)
	api, err := newGoogleAPI(ctx, c.auth)
	if err != nil {
		return nil, err
	}
	if location == "" {
		location, err = api.getDatasetLocation(ctx, project, datasetID)
		if err != nil {
			return nil, fmt.Errorf("read BigQuery dataset location: %w", err)
		}
		if location == "" {
			location = "US"
		}
	}
	jobCallback := func(event map[string]any) {
		_ = c.store.recordJob(project, opID, event)
		if onEvent != nil {
			if event["stage"] == "completed" {
				emitProtocolEvent(onEvent, "billing.job_completed", map[string]any{"job_id": event["job_id"], "operation": event["operation"], "bytes_processed": event["bytes_processed"], "bytes_billed": event["bytes_billed"], "cache_hit": false})
			} else if event["stage"] == "submitted" {
				emitProtocolEvent(onEvent, "billing.job_submitted", map[string]any{"job_id": event["job_id"], "operation": event["operation"], "status": "pending"})
			}
		}
	}
	if boolValue(params["refresh"]) {
		initTime = ""
	}
	if initTime == "" {
		since := now.Add(-48 * time.Hour)
		discovery := discoverySQL(table, cycle)
		rows, _, _, _, queryErr := c.queryWithSpendCap(ctx, api, project, discovery, location, settings, []queryParam{{Name: "since", Type: "TIMESTAMP", Value: since}, {Name: "now", Type: "TIMESTAMP", Value: now}}, jobCallback, "run_discovery")
		if queryErr != nil {
			return nil, queryErr
		}
		if len(rows) == 0 || rows[0]["init_time"] == nil {
			return nil, errors.New("no recent WeatherNext forecast run was found")
		}
		initTime = fmt.Sprint(rows[0]["init_time"])
		if parsed, parseErr := time.Parse(time.RFC3339, initTime); parseErr == nil {
			initTime = parsed.UTC().Format(time.RFC3339Nano)
		}
		_ = c.store.putRun(project, cacheDataset, "surface", cycle, initTime, 5*time.Minute)
	}
	var tableMeta map[string]any
	tableMeta, err = api.getTable(ctx, project, datasetID, tableName)
	if err != nil {
		return nil, err
	}
	forecastFields := []any{}
	if schema, ok := tableMeta["schema"].(map[string]any); ok {
		if fields, ok := schema["fields"].([]any); ok {
			for _, raw := range fields {
				field, ok := raw.(map[string]any)
				if !ok || stringValue(field["name"]) != "forecast" {
					continue
				}
				if nested, ok := field["fields"].([]any); ok {
					forecastFields = nested
				}
				break
			}
		}
	}
	if len(forecastFields) == 0 {
		return nil, errors.New("the selected BigQuery table has no forecast field schema")
	}
	output["products"].(map[string]any)["surface"] = map[string]any{"table": table, "fields": forecastFields}
	initParsed, err := time.Parse(time.RFC3339Nano, initTime)
	if err != nil {
		initParsed, err = time.Parse(time.RFC3339, initTime)
		if err != nil {
			return nil, fmt.Errorf("invalid run timestamp from BigQuery: %w", err)
		}
	}
	misses := make([]int, 0, len(locations))
	usedCache, usedNetwork := false, false
	for i, point := range locations {
		cell := expectedCell(floatValue(point["latitude"]), floatValue(point["longitude"]))
		var cached map[string]any
		if !boolValue(params["refresh"]) {
			cached = c.store.getForecast(project, cacheDataset, "surface", initTime, cell, hours, "retrieval")
		}
		if cached != nil && cachedForecastCovers(cached, start, end) && containsPoint(cached["grid_polygon"], floatValue(point["latitude"]), floatValue(point["longitude"])) {
			selected := sliceCachedForecast(cached, start, end)
			if selected != nil {
				outLocations[i].(map[string]any)["products"].(map[string]any)["surface"] = selected
				usedCache = true
				continue
			}
		}
		misses = append(misses, i)
	}
	if len(misses) > 0 {
		usedNetwork = true
		if onEvent != nil {
			emitProtocolEvent(onEvent, "progress", map[string]any{"stage": "forecast_retrieval", "product": "surface", "locations": len(misses), "total_locations": len(locations)})
		}
		idsForSQL := make([]int, len(misses))
		queryParams := []queryParam{{Name: "now", Type: "TIMESTAMP", Value: now}, {Name: "init_time", Type: "TIMESTAMP", Value: initParsed}, {Name: "window_start", Type: "TIMESTAMP", Value: start}, {Name: "window_end", Type: "TIMESTAMP", Value: end}, {Name: "future_only", Type: "BOOL", Value: true}}
		for j, index := range misses {
			idsForSQL[j] = index
			queryParams = append(queryParams, queryParam{Name: fmt.Sprintf("latitude_%d", index), Type: "FLOAT64", Value: floatValue(locations[index]["latitude"])}, queryParam{Name: fmt.Sprintf("longitude_%d", index), Type: "FLOAT64", Value: floatValue(locations[index]["longitude"])})
		}
		sqlText := forecastSQL(table, idsForSQL)
		rows, _, _, _, queryErr := c.queryWithSpendCap(ctx, api, project, sqlText, location, settings, queryParams, jobCallback, "surface.retrieval")
		if queryErr != nil {
			return nil, queryErr
		}
		found := map[int]bool{}
		for _, row := range rows {
			index := int(parseInt64(fmt.Sprint(row["id"])))
			if index < 0 || index >= len(locations) {
				continue
			}
			found[index] = true
			var center, polygon map[string]any
			var forecasts []any
			if json.Unmarshal([]byte(stringValue(row["grid_center"])), &center) != nil || json.Unmarshal([]byte(stringValue(row["grid_polygon"])), &polygon) != nil {
				return nil, errors.New("BigQuery returned invalid grid geometry")
			}
			if err := json.Unmarshal([]byte(stringValue(row["forecast_json"])), &forecasts); err != nil {
				return nil, errors.New("BigQuery returned unreadable forecast values")
			}
			if len(forecasts) == 0 {
				return nil, fmt.Errorf("no predictions were found for %s", locations[index]["name"])
			}
			distance, _ := strconv.ParseFloat(fmt.Sprint(row["distance_m"]), 64)
			payload := map[string]any{"init_time_utc": initTime, "requested_window_start_utc": start.Format(time.RFC3339Nano), "requested_window_end_utc": end.Format(time.RFC3339Nano), "grid_center": center, "grid_polygon": polygon, "distance_to_grid_center_m": distance, "forecast": forecasts}
			outLocations[index].(map[string]any)["products"].(map[string]any)["surface"] = payload
			coords, _ := center["coordinates"].([]any)
			if len(coords) >= 2 {
				gridLon, _ := floatValueOK(coords[0])
				gridLat, _ := floatValueOK(coords[1])
				_ = c.store.putForecast(project, cacheDataset, "surface", initTime, gridCell(gridLat, gridLon), floatValue(locations[index]["latitude"]), floatValue(locations[index]["longitude"]), hours, "retrieval", payload)
			}
		}
		for _, index := range misses {
			if !found[index] {
				return nil, fmt.Errorf("no WeatherNext grid cell was found for %s", locations[index]["name"])
			}
		}
	}
	path := stringValue(settings["output_path"])
	if path == "" {
		path = filepath.Join(c.home, "output", "forecast.json")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.home, path)
	}
	latest := filepath.Join(c.home, ".local", "weathernext", "forecast-latest.json")
	if err := writeJSONAtomic(latest, output); err != nil {
		return nil, err
	}
	if filepath.Clean(path) != filepath.Clean(latest) {
		if err := writeJSONAtomic(path, output); err != nil {
			return nil, err
		}
	}
	c.store.finishOperation(opID, "completed", nil)
	done = true
	cacheStatus := "miss"
	if usedCache && !usedNetwork {
		cacheStatus = "hit"
	} else if usedCache {
		cacheStatus = "mixed"
	}
	return map[string]any{"output": output, "path": path, "cache_status": cacheStatus}, nil
}

func (c *NativeClient) queryWithSpendCap(ctx context.Context, api *googleAPI, project, sqlText, location string, settings map[string]any, params []queryParam, callback func(map[string]any), operation string) ([]map[string]any, int64, int64, string, error) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	cap, err := c.queryByteCap(settings)
	if err != nil {
		return nil, 0, 0, "", err
	}
	return api.query(ctx, project, sqlText, location, cap, params, callback, operation)
}

func (c *NativeClient) queryByteCap(settings map[string]any) (int64, error) {
	cap := intValueOrZero(settings["max_bytes_billed"])
	for _, item := range []struct{ period, key string }{{"daily", "max_daily_billed_bytes"}, {"monthly", "max_monthly_billed_bytes"}} {
		limit, ok := intValue(settings[item.key])
		if !ok || limit <= 0 {
			continue
		}
		current, err := c.store.reportedBytesForPeriod(item.period, time.Now().UTC())
		if err != nil {
			return 0, err
		}
		remaining := limit - current
		if remaining <= 0 {
			return 0, fmt.Errorf("local %s BigQuery billed-byte limit reached (%s of %s bytes); no new query was started", item.period, formatInteger(current), formatInteger(limit))
		}
		if cap <= 0 || remaining < cap {
			cap = remaining
		}
	}
	return cap, nil
}

func emitProtocolEvent(handler func(protocol.Message), name string, data any) {
	encoded, _ := json.Marshal(data)
	handler(protocol.Message{Version: protocol.Version, Type: "event", Event: name, Data: encoded})
}

func boolValue(value any) bool { result, _ := value.(bool); return result }

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func formatInteger(value int64) string {
	if value < 1000 {
		return fmt.Sprint(value)
	}
	s := fmt.Sprint(value)
	var out strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(ch)
	}
	return out.String()
}

func cachedForecastCovers(payload map[string]any, start, end time.Time) bool {
	forecast, ok := payload["forecast"].([]any)
	if !ok || len(forecast) == 0 {
		return false
	}
	last, ok := forecast[len(forecast)-1].(map[string]any)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339, stringValue(last["time"]))
	if err != nil {
		return false
	}
	return !at.Before(end.Truncate(time.Hour))
}

func sliceCachedForecast(payload map[string]any, start, end time.Time) map[string]any {
	copy := map[string]any{}
	for key, value := range payload {
		copy[key] = value
	}
	raw, ok := payload["forecast"].([]any)
	if !ok {
		return nil
	}
	selected := []any{}
	for _, item := range raw {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, stringValue(record["time"]))
		if err == nil && !at.Before(start) && !at.After(end) {
			selected = append(selected, record)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	copy["requested_window_start_utc"] = start.Format(time.RFC3339Nano)
	copy["requested_window_end_utc"] = end.Format(time.RFC3339Nano)
	copy["forecast"] = selected
	return copy
}

func containsPoint(value any, lat, lon float64) bool {
	polygon, ok := value.(map[string]any)
	if !ok {
		return false
	}
	typ := stringValue(polygon["type"])
	coords, ok := polygon["coordinates"].([]any)
	if !ok {
		return false
	}
	polygons := [][]any{}
	if typ == "Polygon" {
		polygons = append(polygons, coords)
	} else if typ == "MultiPolygon" {
		for _, raw := range coords {
			rings, ok := raw.([]any)
			if ok {
				polygons = append(polygons, rings)
			}
		}
	} else {
		return false
	}
	for _, rings := range polygons {
		if len(rings) == 0 {
			continue
		}
		outer, _ := rings[0].([]any)
		if pointInRing(outer, lon, lat) {
			hole := false
			for _, raw := range rings[1:] {
				ring, _ := raw.([]any)
				if pointInRing(ring, lon, lat) {
					hole = true
					break
				}
			}
			if !hole {
				return true
			}
		}
	}
	return false
}

func pointInRing(ring []any, lon, lat float64) bool {
	if len(ring) < 3 {
		return false
	}
	inside := false
	prev, _ := ring[len(ring)-1].([]any)
	for _, raw := range ring {
		cur, _ := raw.([]any)
		if len(cur) < 2 || len(prev) < 2 {
			prev = cur
			continue
		}
		x1, _ := floatValueOK(prev[0])
		y1, _ := floatValueOK(prev[1])
		x2, _ := floatValueOK(cur[0])
		y2, _ := floatValueOK(cur[1])
		cross := (lon-x1)*(y2-y1) - (lat-y1)*(x2-x1)
		if math.Abs(cross) < 1e-10 && lon >= math.Min(x1, x2)-1e-10 && lon <= math.Max(x1, x2)+1e-10 && lat >= math.Min(y1, y2)-1e-10 && lat <= math.Max(y1, y2)+1e-10 {
			return true
		}
		if (y1 > lat) != (y2 > lat) {
			edge := (x2-x1)*(lat-y1)/(y2-y1) + x1
			if lon < edge {
				inside = !inside
			}
		}
		prev = cur
	}
	return inside
}
