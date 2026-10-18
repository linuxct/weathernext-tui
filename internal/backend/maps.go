package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	geocodingURL     = "https://maps.googleapis.com/maps/api/geocode/json"
	geocodingService = "geocoding-backend.googleapis.com"
	apiKeysService   = "apikeys.googleapis.com"
)

type managedMapsKey struct {
	Name    string `json:"name"`
	Value   string `json:"key_string"`
	Project string `json:"project_id"`
}

func (c *NativeClient) mapsKey(project string) string {
	path := filepath.Join(c.home, ".local", "weathernext", "maps_api_key.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var key managedMapsKey
	if json.Unmarshal(data, &key) != nil || key.Project != project || !strings.HasPrefix(key.Value, "AIza") {
		return ""
	}
	return key.Value
}

func (c *NativeClient) saveMapsKey(key managedMapsKey) error {
	path := filepath.Join(c.home, ".local", "weathernext", "maps_api_key.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(key, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (c *NativeClient) setupMaps(ctx context.Context) map[string]any {
	project := strings.TrimSpace(stringValue(c.store.getSettingsMap("app")["target_project"]))
	if project == "" {
		return map[string]any{"configured": false, "message": "Enter the Google Cloud project ID in Settings to set up location search."}
	}
	if c.mapsKey(project) != "" {
		return map[string]any{"configured": true, "source": "managed", "message": "Google Maps location search is ready."}
	}
	client, err := c.auth.httpClient(ctx)
	if err != nil {
		return map[string]any{"configured": false, "message": "Sign in with Google before setting up location search."}
	}
	projectPath := url.PathEscape(project)
	for _, service := range []string{apiKeysService, geocodingService} {
		if err := enableGoogleService(ctx, client, projectPath, service); err != nil {
			return map[string]any{"configured": false, "message": safeMapsSetupError(err)}
		}
	}
	parent := "projects/" + projectPath + "/locations/global"
	base := "https://apikeys.googleapis.com/v2/" + parent + "/keys"
	var page struct {
		Keys []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"keys"`
	}
	if err := googleJSON(ctx, client, http.MethodGet, base+"?pageSize=100", nil, &page); err != nil {
		return map[string]any{"configured": false, "message": safeMapsSetupError(err)}
	}
	keyName := ""
	for _, key := range page.Keys {
		if key.DisplayName == "WeatherNext TUI Geocoding" {
			keyName = key.Name
			break
		}
	}
	if keyName == "" {
		var operation struct {
			Name     string `json:"name"`
			Done     bool   `json:"done"`
			Response struct {
				Name string `json:"name"`
			} `json:"response"`
			Error any `json:"error"`
		}
		body := map[string]any{"displayName": "WeatherNext TUI Geocoding", "restrictions": map[string]any{"apiTargets": []any{map[string]any{"service": geocodingService}}}}
		if err := googleJSON(ctx, client, http.MethodPost, base+"?keyId=weathernext-tui-geocoding", body, &operation); err != nil {
			return map[string]any{"configured": false, "message": safeMapsSetupError(err)}
		}
		if operation.Done && operation.Response.Name != "" {
			keyName = operation.Response.Name
		} else if operation.Name != "" {
			keyName, err = pollMapsOperation(ctx, client, "https://apikeys.googleapis.com/v2/", operation.Name)
			if err != nil {
				return map[string]any{"configured": false, "message": safeMapsSetupError(err)}
			}
		} else {
			return map[string]any{"configured": false, "message": "Google did not return a Maps key resource."}
		}
	}
	var keyResult struct {
		KeyString string `json:"keyString"`
	}
	if err := googleJSON(ctx, client, http.MethodGet, "https://apikeys.googleapis.com/v2/"+keyName+"/keyString", nil, &keyResult); err != nil {
		return map[string]any{"configured": false, "message": safeMapsSetupError(err)}
	}
	if !strings.HasPrefix(keyResult.KeyString, "AIza") {
		return map[string]any{"configured": false, "message": "Google returned an invalid Maps key."}
	}
	if err := c.saveMapsKey(managedMapsKey{Name: keyName, Value: keyResult.KeyString, Project: project}); err != nil {
		return map[string]any{"configured": false, "message": "Could not save the restricted Maps key locally."}
	}
	return map[string]any{"configured": true, "source": "managed", "message": "Google Maps location search is ready."}
}

func enableGoogleService(ctx context.Context, client *http.Client, project, service string) error {
	endpoint := "https://serviceusage.googleapis.com/v1/projects/" + project + "/services/" + service
	var state struct {
		State string `json:"state"`
	}
	if err := googleJSON(ctx, client, http.MethodGet, endpoint, nil, &state); err == nil && state.State == "ENABLED" {
		return nil
	}
	var operation struct {
		Name  string `json:"name"`
		Done  bool   `json:"done"`
		Error any    `json:"error"`
	}
	if err := googleJSON(ctx, client, http.MethodPost, endpoint+":enable", map[string]any{}, &operation); err != nil {
		return err
	}
	if operation.Done {
		if operation.Error != nil {
			return fmt.Errorf("enable API failed")
		}
		return nil
	}
	if operation.Name == "" {
		return errors.New("Google did not return an API enable operation")
	}
	_, err := pollOperation(ctx, client, "https://serviceusage.googleapis.com/v1/", operation.Name)
	return err
}

func pollMapsOperation(ctx context.Context, client *http.Client, base, name string) (string, error) {
	for i := 0; i < 60; i++ {
		var operation struct {
			Done     bool `json:"done"`
			Error    any  `json:"error"`
			Response struct {
				Name string `json:"name"`
			} `json:"response"`
		}
		if err := googleJSON(ctx, client, http.MethodGet, base+name, nil, &operation); err != nil {
			return "", err
		}
		if operation.Done {
			if operation.Error != nil {
				return "", errors.New("Google Maps key creation failed")
			}
			return operation.Response.Name, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", errors.New("Google Maps setup is still running; retry in a moment")
}

func pollOperation(ctx context.Context, client *http.Client, base, name string) (map[string]any, error) {
	for i := 0; i < 60; i++ {
		var operation map[string]any
		if err := googleJSON(ctx, client, http.MethodGet, base+name, nil, &operation); err != nil {
			return nil, err
		}
		if boolValue(operation["done"]) {
			if operation["error"] != nil {
				return nil, errors.New("Google service enable operation failed")
			}
			return operation, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, errors.New("Google API setup is still running; retry in a moment")
}

func googleJSON(ctx context.Context, client *http.Client, method, endpoint string, input, output any) error {
	var body strings.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = *strings.NewReader(string(data))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, &body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&result)
		return fmt.Errorf("google api returned %d %s", response.StatusCode, result.Error.Status)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(output)
}

func safeMapsSetupError(err error) string {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "403") || strings.Contains(message, "permission_denied") {
		return "Google sign-in succeeded, but this account needs Service Usage Admin and API Keys Admin access to set up location search."
	}
	if strings.Contains(message, "billing") {
		return "Google Maps setup needs billing enabled for this project."
	}
	return "Google Maps setup failed. Check that billing is enabled and retry from Settings."
}

func (c *NativeClient) searchLocations(ctx context.Context, params map[string]any) (map[string]any, error) {
	query := strings.TrimSpace(stringValue(params["query"]))
	if query == "" {
		return mapsFailure("error", "Enter a place name to search."), nil
	}
	if len(query) > 512 {
		return mapsFailure("error", "Place name is too long."), nil
	}
	project := strings.TrimSpace(stringValue(c.store.getSettingsMap("app")["target_project"]))
	key := c.mapsKey(project)
	if key == "" {
		setup := c.setupMaps(ctx)
		if !boolValue(setup["configured"]) {
			return mapsFailure("needs_key", stringValue(setup["message"])), nil
		}
		key = c.mapsKey(project)
	}
	return c.geocode(ctx, key, url.Values{"address": {query}}, "")
}

func (c *NativeClient) resolveLocation(ctx context.Context, params map[string]any) (map[string]any, error) {
	id := stringValue(params["id"])
	location, err := c.store.location(id)
	if err != nil {
		return nil, errors.New("saved location was not found")
	}
	placeID := stringValue(location["place_id"])
	if placeID == "" {
		return nil, errors.New("this saved location has no Google place ID")
	}
	project := strings.TrimSpace(stringValue(c.store.getSettingsMap("app")["target_project"]))
	key := c.mapsKey(project)
	if key == "" {
		setup := c.setupMaps(ctx)
		if !boolValue(setup["configured"]) {
			return mapsFailure("needs_key", stringValue(setup["message"])), nil
		}
		key = c.mapsKey(project)
	}
	return c.geocode(ctx, key, url.Values{"place_id": {placeID}}, placeID)
}

func (c *NativeClient) geocode(ctx context.Context, key string, query url.Values, expectedPlaceID string) (map[string]any, error) {
	query.Set("key", key)
	endpoint := geocodingURL + "?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "WeatherNextViewer/1.0")
	response, requestErr := http.DefaultClient.Do(request)
	record := map[string]any{"recorded_at_utc": utcNow(), "status": "error"}
	if rate, ok := floatValueOK(c.store.getSettingsMap("billing")["maps_usd_per_1000_requests"]); ok && rate >= 0 {
		record["estimated_cost"] = rate / 1000
		record["currency"] = "USD"
	}
	result := map[string]any{}
	if requestErr != nil {
		result = mapsFailure("error", "Could not reach Google Maps. Check the connection and try again.")
	} else {
		defer response.Body.Close()
		var payload struct {
			Status  string `json:"status"`
			Results []struct {
				Address  string `json:"formatted_address"`
				PlaceID  string `json:"place_id"`
				Geometry struct {
					Location struct {
						Lat float64 `json:"lat"`
						Lng float64 `json:"lng"`
					} `json:"location"`
				} `json:"geometry"`
			} `json:"results"`
		}
		if json.NewDecoder(response.Body).Decode(&payload) != nil {
			result = mapsFailure("error", "Google Maps returned unreadable data.")
		} else {
			switch payload.Status {
			case "OK":
				candidates := []any{}
				for _, item := range payload.Results {
					if expectedPlaceID != "" && item.PlaceID != expectedPlaceID {
						continue
					}
					candidates = append(candidates, map[string]any{"name": item.Address, "latitude": item.Geometry.Location.Lat, "longitude": item.Geometry.Location.Lng, "place_id": item.PlaceID, "attribution": "Google Maps"})
				}
				if len(candidates) == 0 {
					result = mapsFailure("no_results", "No matching places were found.")
				} else {
					status := "ok"
					if len(candidates) > 1 {
						status = "ambiguous"
					}
					result = map[string]any{"status": status, "candidates": candidates, "attribution": "Google Maps"}
					if expectedPlaceID != "" {
						result["candidate"] = candidates[0]
					}
				}
			case "ZERO_RESULTS":
				result = mapsFailure("no_results", "No matching places were found.")
			case "OVER_QUERY_LIMIT", "OVER_DAILY_LIMIT", "RESOURCE_EXHAUSTED":
				result = mapsFailure("quota", "Google Maps request quota was reached.")
			case "REQUEST_DENIED", "PERMISSION_DENIED":
				result = mapsFailure("denied", "Google Maps denied the request. Check that billing and Geocoding API access are enabled.")
			default:
				result = mapsFailure("error", "Google Maps could not complete the search.")
			}
		}
	}
	status := stringValue(result["status"])
	record["status"] = status
	if expectedPlaceID != "" {
		record["place_id"] = expectedPlaceID
	}
	_ = c.store.recordMapsRequest(record)
	return result, nil
}

func mapsFailure(status, message string) map[string]any {
	return map[string]any{"status": status, "message": message, "candidates": []any{}, "attribution": "Google Maps"}
}

func (c *NativeClient) saveLocation(params map[string]any) (map[string]any, error) {
	name := strings.TrimSpace(stringValue(params["name"]))
	lat, latOK := floatValueOK(params["latitude"])
	lon, lonOK := floatValueOK(params["longitude"])
	if name == "" || len(name) > 256 {
		return nil, errors.New("enter a location name")
	}
	if !latOK || !lonOK || math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, errors.New("location coordinates are invalid")
	}
	id := stringValue(params["id"])
	if id == "" {
		id = randomIDText()
	}
	value := map[string]any{"id": id, "name": name, "latitude": lat, "longitude": lon, "place_id": stringValue(params["place_id"]), "attribution": stringValue(params["attribution"])}
	if value["place_id"] != "" {
		value["geocoded_at_utc"] = utcNow()
	}
	if err := c.store.saveLocation(value); err != nil {
		return nil, err
	}
	location, err := c.store.location(id)
	return map[string]any{"location": location}, err
}
