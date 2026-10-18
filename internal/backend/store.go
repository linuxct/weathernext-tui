package backend

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type store struct{ db *sql.DB }

func openStore(home string) (*store, error) {
	path := filepath.Join(home, ".local", "weathernext", "weathernext.sqlite3")
	if err := ensureParent(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open local WeatherNext database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at_utc TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS locations (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, latitude REAL, longitude REAL,
 place_id TEXT, attribution TEXT, geocoded_at_utc TEXT,
 created_at_utc TEXT NOT NULL, updated_at_utc TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS forecast_cache (
 project TEXT NOT NULL, dataset TEXT NOT NULL, product TEXT NOT NULL,
 init_time_utc TEXT NOT NULL, grid_cell TEXT NOT NULL, latitude REAL NOT NULL,
 longitude REAL NOT NULL, requested_hours INTEGER NOT NULL, window_reference TEXT NOT NULL,
 payload_json TEXT NOT NULL, stored_at_utc TEXT NOT NULL,
 PRIMARY KEY(project,dataset,product,init_time_utc,grid_cell,requested_hours,window_reference));
CREATE TABLE IF NOT EXISTS dismissed_locations (coordinate_key TEXT PRIMARY KEY);
CREATE INDEX IF NOT EXISTS forecast_cache_recent ON forecast_cache(stored_at_utc DESC);
CREATE TABLE IF NOT EXISTS run_cache (
 project TEXT NOT NULL, dataset TEXT NOT NULL, product TEXT NOT NULL, cycle TEXT NOT NULL,
 init_time_utc TEXT NOT NULL, expires_at_utc TEXT NOT NULL, stored_at_utc TEXT NOT NULL,
 PRIMARY KEY(project,dataset,product,cycle));
CREATE TABLE IF NOT EXISTS operations (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL, started_at_utc TEXT NOT NULL,
 finished_at_utc TEXT, error TEXT);
CREATE TABLE IF NOT EXISTS bq_jobs (
 project TEXT NOT NULL, job_id TEXT NOT NULL, operation_id TEXT, operation TEXT NOT NULL,
 status TEXT NOT NULL, bytes_processed INTEGER, bytes_billed INTEGER,
 submitted_at_utc TEXT NOT NULL, completed_at_utc TEXT, error TEXT,
 PRIMARY KEY(project,job_id));
CREATE INDEX IF NOT EXISTS bq_jobs_submitted ON bq_jobs(submitted_at_utc DESC);
CREATE TABLE IF NOT EXISTS maps_requests (
 id INTEGER PRIMARY KEY AUTOINCREMENT, recorded_at_utc TEXT NOT NULL,
 status TEXT NOT NULL, place_id TEXT, estimated_cost TEXT, currency TEXT);
CREATE TABLE IF NOT EXISTS settings (
 key TEXT PRIMARY KEY, value_json TEXT NOT NULL, updated_at_utc TEXT NOT NULL);
INSERT OR IGNORE INTO schema_migrations(version,applied_at_utc) VALUES(1,CURRENT_TIMESTAMP);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize local WeatherNext database: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error { return s.db.Close() }

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

func (s *store) getSetting(key string, fallback any) any {
	var raw string
	if err := s.db.QueryRow("SELECT value_json FROM settings WHERE key=?", key).Scan(&raw); err != nil {
		return fallback
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return fallback
	}
	return value
}

func (s *store) updateSettings(values map[string]any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(key,value_json,updated_at_utc) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at_utc=excluded.updated_at_utc`,
			key, string(data), utcNow()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) getSettingsMap(key string) map[string]any {
	value, _ := s.getSetting(key, map[string]any{}).(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}

func (s *store) recordJob(project, operationID string, event map[string]any) error {
	jobID := stringValue(event["job_id"])
	if jobID == "" {
		return nil
	}
	stage := stringValue(event["stage"])
	operation := stringValue(event["operation"])
	if operation == "" {
		operation = "query"
	}
	status := map[string]string{"submitted": "pending", "completed": "completed", "failed": "failed"}[stage]
	if status == "" {
		status = stage
	}
	now := utcNow()
	processed, hasProcessed := intValue(event["bytes_processed"])
	billed, hasBilled := intValue(event["bytes_billed"])
	if stage == "submitted" {
		_, err := s.db.Exec(`INSERT INTO bq_jobs(project,job_id,operation_id,operation,status,submitted_at_utc)
VALUES(?,?,?,?,?,?) ON CONFLICT(project,job_id) DO NOTHING`, project, jobID, operationID, operation, status, now)
		return err
	}
	var p, b any
	if hasProcessed {
		p = processed
	}
	if hasBilled {
		b = billed
	}
	_, err := s.db.Exec(`INSERT INTO bq_jobs(project,job_id,operation_id,operation,status,bytes_processed,bytes_billed,submitted_at_utc,completed_at_utc,error)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project,job_id) DO UPDATE SET status=excluded.status,
bytes_processed=excluded.bytes_processed,bytes_billed=excluded.bytes_billed,
completed_at_utc=excluded.completed_at_utc,error=excluded.error`, project, jobID, operationID,
		operation, status, p, b, now, now, event["error"])
	return err
}

func (s *store) recordMapsRequest(record map[string]any) error {
	var cost any
	if value, ok := record["estimated_cost"]; ok && value != nil {
		cost = fmt.Sprint(value)
	}
	_, err := s.db.Exec(`INSERT INTO maps_requests(recorded_at_utc,status,place_id,estimated_cost,currency) VALUES(?,?,?,?,?)`,
		firstString(stringValue(record["recorded_at_utc"]), utcNow()), stringValue(record["status"]),
		nilIfEmpty(stringValue(record["place_id"])), cost, nilIfEmpty(stringValue(record["currency"])))
	return err
}

func (s *store) startOperation(kind, id string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO operations(id,kind,status,started_at_utc) VALUES(?,?,?,?)`, id, kind, "running", utcNow())
	return err
}

func (s *store) finishOperation(id, status string, cause error) {
	var message any
	if cause != nil {
		message = cause.Error()
	}
	_, _ = s.db.Exec(`UPDATE operations SET status=?,finished_at_utc=?,error=? WHERE id=?`, status, utcNow(), message, id)
}

func (s *store) putRun(project, dataset, product, cycle, init string, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO run_cache(project,dataset,product,cycle,init_time_utc,expires_at_utc,stored_at_utc)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(project,dataset,product,cycle) DO UPDATE SET init_time_utc=excluded.init_time_utc,
expires_at_utc=excluded.expires_at_utc,stored_at_utc=excluded.stored_at_utc`,
		project, dataset, product, cycle, init, now.Add(ttl).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return err
}

func (s *store) getRun(project, dataset, product, cycle string) string {
	var init, expires string
	if s.db.QueryRow(`SELECT init_time_utc,expires_at_utc FROM run_cache WHERE project=? AND dataset=? AND product=? AND cycle=?`,
		project, dataset, product, cycle).Scan(&init, &expires) != nil {
		return ""
	}
	deadline, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || time.Now().UTC().After(deadline) {
		return ""
	}
	return init
}

func (s *store) putForecast(project, dataset, product, init, cell string, lat, lon float64, hours int, reference string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO forecast_cache VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project,dataset,product,init_time_utc,grid_cell,requested_hours,window_reference)
DO UPDATE SET latitude=excluded.latitude,longitude=excluded.longitude,payload_json=excluded.payload_json,stored_at_utc=excluded.stored_at_utc`,
		project, dataset, product, init, cell, lat, lon, hours, reference, string(data), utcNow())
	return err
}

func (s *store) getForecast(project, dataset, product, init, cell string, hours int, reference string) map[string]any {
	var raw string
	err := s.db.QueryRow(`SELECT payload_json FROM forecast_cache WHERE project=? AND dataset=? AND product=?
AND init_time_utc=? AND grid_cell=? AND requested_hours>=? AND window_reference=? ORDER BY requested_hours ASC LIMIT 1`,
		project, dataset, product, init, cell, hours, reference).Scan(&raw)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return nil
	}
	return payload
}

func (s *store) forecastCount() int {
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM forecast_cache").Scan(&count)
	return count
}

func (s *store) listLocations() ([]map[string]any, error) {
	rows, err := s.db.Query("SELECT id,name,latitude,longitude,place_id,attribution,geocoded_at_utc,created_at_utc,updated_at_utc FROM locations ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, name, created, updated string
		var lat, lon sql.NullFloat64
		var place, attribution, geocoded sql.NullString
		if err := rows.Scan(&id, &name, &lat, &lon, &place, &attribution, &geocoded, &created, &updated); err != nil {
			return nil, err
		}
		location := map[string]any{"id": id, "name": name, "created_at_utc": created, "updated_at_utc": updated}
		if lat.Valid {
			location["latitude"] = lat.Float64
		} else {
			location["latitude"] = nil
		}
		if lon.Valid {
			location["longitude"] = lon.Float64
		} else {
			location["longitude"] = nil
		}
		if place.Valid {
			location["place_id"] = place.String
		}
		if attribution.Valid {
			location["attribution"] = attribution.String
		}
		if geocoded.Valid {
			location["geocoded_at_utc"] = geocoded.String
		}
		if place.Valid && !coordinatesFresh(geocoded.String) {
			location["latitude"], location["longitude"], location["coordinates_expired"] = nil, nil, true
		}
		result = append(result, location)
	}
	return result, rows.Err()
}

func (s *store) location(id string) (map[string]any, error) {
	var name, created, updated string
	var lat, lon sql.NullFloat64
	var place, attribution, geocoded sql.NullString
	err := s.db.QueryRow(`SELECT name,latitude,longitude,place_id,attribution,geocoded_at_utc,created_at_utc,updated_at_utc FROM locations WHERE id=?`, id).
		Scan(&name, &lat, &lon, &place, &attribution, &geocoded, &created, &updated)
	if err != nil {
		return nil, err
	}
	value := map[string]any{"id": id, "name": name, "created_at_utc": created, "updated_at_utc": updated}
	if lat.Valid {
		value["latitude"] = lat.Float64
	}
	if lon.Valid {
		value["longitude"] = lon.Float64
	}
	if place.Valid {
		value["place_id"] = place.String
	}
	if attribution.Valid {
		value["attribution"] = attribution.String
	}
	if geocoded.Valid {
		value["geocoded_at_utc"] = geocoded.String
	}
	return value, nil
}

func (s *store) saveLocation(value map[string]any) error {
	now := utcNow()
	_, err := s.db.Exec(`INSERT INTO locations(id,name,latitude,longitude,place_id,attribution,geocoded_at_utc,created_at_utc,updated_at_utc)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,latitude=excluded.latitude,longitude=excluded.longitude,
place_id=excluded.place_id,attribution=excluded.attribution,geocoded_at_utc=excluded.geocoded_at_utc,updated_at_utc=excluded.updated_at_utc`,
		value["id"], value["name"], value["latitude"], value["longitude"], nilIfEmpty(stringValue(value["place_id"])),
		nilIfEmpty(stringValue(value["attribution"])), nilIfEmpty(stringValue(value["geocoded_at_utc"])), now, now)
	if err == nil && value["latitude"] != nil && value["longitude"] != nil {
		_, err = s.db.Exec("DELETE FROM dismissed_locations WHERE coordinate_key=?", coordinateKey(floatValue(value["latitude"]), floatValue(value["longitude"])))
	}
	if err == nil {
		err = s.updateSettings(map[string]any{"locations_empty_by_choice": false})
	}
	return err
}

func (s *store) deleteLocation(id string) (bool, error) {
	value, err := s.location(id)
	if err == nil && value["latitude"] != nil && value["longitude"] != nil {
		if _, err = s.db.Exec("INSERT OR IGNORE INTO dismissed_locations(coordinate_key) VALUES(?)", coordinateKey(floatValue(value["latitude"]), floatValue(value["longitude"]))); err != nil {
			return false, err
		}
	}
	result, err := s.db.Exec("DELETE FROM locations WHERE id=?", id)
	if err != nil {
		return false, err
	}
	deleted, _ := result.RowsAffected()
	if deleted > 0 {
		var remaining int
		_ = s.db.QueryRow("SELECT COUNT(*) FROM locations").Scan(&remaining)
		if remaining == 0 {
			_ = s.updateSettings(map[string]any{"locations_empty_by_choice": true})
		}
	}
	return deleted > 0, nil
}

func (s *store) jobs(limit int) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT project,job_id,operation_id,operation,status,bytes_processed,bytes_billed,submitted_at_utc,completed_at_utc,error
FROM bq_jobs ORDER BY submitted_at_utc DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var project, id, operation, status, submitted string
		var operationID, completed, failure sql.NullString
		var processed, billed sql.NullInt64
		if err := rows.Scan(&project, &id, &operationID, &operation, &status, &processed, &billed, &submitted, &completed, &failure); err != nil {
			return nil, err
		}
		job := map[string]any{"project": project, "job_id": id, "operation": operation, "status": status, "submitted_at_utc": submitted}
		if operationID.Valid {
			job["operation_id"] = operationID.String
		}
		if processed.Valid {
			job["bytes_processed"] = processed.Int64
		}
		if billed.Valid {
			job["bytes_billed"] = billed.Int64
		}
		if completed.Valid {
			job["completed_at_utc"] = completed.String
		}
		if failure.Valid {
			job["error"] = failure.String
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *store) spendingSummary() (map[string]any, error) {
	var total sql.NullInt64
	var jobs, pending, failed, unknown, known int
	if err := s.db.QueryRow(`SELECT SUM(bytes_billed),COUNT(*),SUM(status='pending'),SUM(status='failed'),SUM(bytes_billed IS NULL),COUNT(bytes_billed) FROM bq_jobs`).
		Scan(&total, &jobs, &pending, &failed, &unknown, &known); err != nil {
		return nil, err
	}
	var mapsCount, mapsSuccess, mapsFailed int
	var mapsCost float64
	_ = s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN estimated_cost IS NOT NULL THEN CAST(estimated_cost AS REAL) ELSE 0 END),0),
SUM(status='ok'),SUM(status IN ('error','denied','quota')) FROM maps_requests`).Scan(&mapsCount, &mapsCost, &mapsSuccess, &mapsFailed)
	var bytesValue any
	if total.Valid {
		bytesValue = total.Int64
	} else if jobs == 0 {
		bytesValue = int64(0)
	}
	billing := s.getSettingsMap("billing")
	app := s.getSettingsMap("app")
	var rate any = billing["usd_per_tib"]
	var estimate any
	if value, ok := floatValueOK(rate); ok && bytesValue != nil {
		estimate = float64(intValueOrZero(bytesValue)) * value / math.Pow(1024, 4)
	}
	daily, err := s.reportedBytesForPeriod("daily", time.Now().UTC())
	if err != nil {
		return nil, err
	}
	monthly, err := s.reportedBytesForPeriod("monthly", time.Now().UTC())
	if err != nil {
		return nil, err
	}
	var gib any
	if bytesValue != nil {
		gib = float64(intValueOrZero(bytesValue)) / math.Pow(1024, 3)
	}
	return map[string]any{
		"reported_bytes_billed": bytesValue, "reported_gib": gib, "estimated_cost_usd": estimate, "estimate_rate_usd_per_tib": rate,
		"jobs": jobs, "pending_jobs": pending, "known_billed_jobs": known, "failed_jobs": failed, "unknown_jobs": unknown, "unresolved_jobs": unknown,
		"maps_requests": mapsCount, "maps_successful_requests": mapsSuccess, "maps_failed_requests": mapsFailed, "maps_estimated_cost_usd": mapsCost,
		"daily_bytes_billed": daily, "daily_byte_limit": app["max_daily_billed_bytes"], "monthly_bytes_billed": monthly, "monthly_byte_limit": app["max_monthly_billed_bytes"],
		"note": "Billed bytes come from BigQuery job results. Each new query uses the lower of its per-query cap and the remaining local daily/monthly byte threshold. Local limits include only jobs started by this app, not other BigQuery work. The price estimate does not deduct free tier.",
	}, nil
}

func (s *store) reportedBytesForPeriod(period string, now time.Time) (int64, error) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	if period == "monthly" {
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	}
	var total sql.NullInt64
	err := s.db.QueryRow(`SELECT SUM(bytes_billed) FROM bq_jobs WHERE status='completed' AND bytes_billed IS NOT NULL AND completed_at_utc>=? AND completed_at_utc<?`,
		start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)).Scan(&total)
	if err != nil {
		return 0, err
	}
	if total.Valid {
		return total.Int64, nil
	}
	return 0, nil
}

func coordinatesFresh(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return false
	}
	age := time.Since(parsed)
	return age >= 0 && age < 30*24*time.Hour
}

func utcNow() string                        { return time.Now().UTC().Format(time.RFC3339Nano) }
func coordinateKey(lat, lon float64) string { return fmt.Sprintf("%.6f:%.6f", lat, lon) }
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func stringValue(value any) string { result, _ := value.(string); return result }
func floatValue(value any) float64 { result, _ := floatValueOK(value); return result }
func floatValueOK(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	default:
		return 0, false
	}
}
func intValue(value any) (int64, bool) {
	number, ok := floatValueOK(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return int64(number), true
}
func intValueOrZero(value any) int64 { result, _ := intValue(value); return result }

func sortedStringKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func sanitizeSettings(values map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range values {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), "_", ""))
		if strings.Contains(normalized, "apikey") || strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "password") || strings.Contains(normalized, "auth") {
			continue
		}
		result[key] = value
	}
	return result
}
