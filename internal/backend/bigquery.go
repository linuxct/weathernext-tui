package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const bigQueryBase = "https://bigquery.googleapis.com/bigquery/v2"

type googleAPI struct {
	http    *http.Client
	baseURL string
}

func (g *googleAPI) base() string {
	if strings.TrimSpace(g.baseURL) != "" {
		return strings.TrimRight(g.baseURL, "/")
	}
	return bigQueryBase
}

type queryParam struct {
	Name  string
	Type  string
	Value any
}

type queryField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type queryCell struct {
	Value any `json:"v"`
}
type queryRow struct {
	Values []queryCell `json:"f"`
}

type queryResponse struct {
	JobComplete  bool `json:"jobComplete"`
	JobReference struct {
		JobID    string `json:"jobId"`
		Location string `json:"location"`
	} `json:"jobReference"`
	Schema struct {
		Fields []queryField `json:"fields"`
	} `json:"schema"`
	Rows                []queryRow `json:"rows"`
	PageToken           string     `json:"pageToken"`
	TotalBytesProcessed string     `json:"totalBytesProcessed"`
	TotalBytesBilled    string     `json:"totalBytesBilled"`
	Errors              []struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"errors"`
}

type queryRequest struct {
	Query              string           `json:"query"`
	UseLegacySQL       bool             `json:"useLegacySql"`
	UseQueryCache      bool             `json:"useQueryCache"`
	Location           string           `json:"location,omitempty"`
	TimeoutMS          int              `json:"timeoutMs"`
	MaximumBytesBilled string           `json:"maximumBytesBilled,omitempty"`
	ParameterMode      string           `json:"parameterMode,omitempty"`
	QueryParameters    []map[string]any `json:"queryParameters,omitempty"`
}

func newGoogleAPI(ctx context.Context, auth *authService) (*googleAPI, error) {
	client, err := auth.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return &googleAPI{http: client}, nil
}

func (g *googleAPI) getTable(ctx context.Context, project, dataset, table string) (map[string]any, error) {
	endpoint := g.base() + "/projects/" + url.PathEscape(project) + "/datasets/" + url.PathEscape(dataset) + "/tables/" + url.PathEscape(table)
	var result map[string]any
	if err := g.request(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (g *googleAPI) getDatasetLocation(ctx context.Context, project, dataset string) (string, error) {
	endpoint := g.base() + "/projects/" + url.PathEscape(project) + "/datasets/" + url.PathEscape(dataset)
	var result struct {
		Location string `json:"location"`
	}
	if err := g.request(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return "", err
	}
	return result.Location, nil
}

func (g *googleAPI) query(ctx context.Context, project, sql, location string, maxBytes int64, params []queryParam,
	jobCallback func(map[string]any), operation string) ([]map[string]any, int64, int64, string, error) {
	request := queryRequest{Query: sql, UseLegacySQL: false, UseQueryCache: true, Location: location, TimeoutMS: 10000}
	if maxBytes > 0 {
		request.MaximumBytesBilled = strconv.FormatInt(maxBytes, 10)
	}
	if len(params) > 0 {
		request.ParameterMode = "NAMED"
		for _, param := range params {
			request.QueryParameters = append(request.QueryParameters, map[string]any{"name": param.Name,
				"parameterType":  map[string]any{"type": param.Type},
				"parameterValue": map[string]any{"value": queryParamString(param.Value)}})
		}
	}
	endpoint := g.base() + "/projects/" + url.PathEscape(project) + "/queries"
	var response queryResponse
	if err := g.request(ctx, http.MethodPost, endpoint, request, &response); err != nil {
		return nil, 0, 0, "", err
	}
	jobID := response.JobReference.JobID
	if jobID != "" && jobCallback != nil {
		jobCallback(map[string]any{"stage": "submitted", "job_id": jobID, "operation": operation})
	}
	if jobID == "" {
		return nil, 0, 0, "", fmt.Errorf("BigQuery did not return a job ID")
	}
	locationValue := firstString(response.JobReference.Location, location)
	for !response.JobComplete {
		select {
		case <-ctx.Done():
			g.cancelJob(project, jobID, locationValue)
			if jobCallback != nil {
				jobCallback(map[string]any{"stage": "cancelled", "job_id": jobID, "operation": operation})
			}
			return nil, 0, 0, jobID, ctx.Err()
		case <-time.After(time.Second):
		}
		pollURL := g.base() + "/projects/" + url.PathEscape(project) + "/queries/" + url.PathEscape(jobID) + "?location=" + url.QueryEscape(locationValue) + "&maxResults=10000"
		if err := g.request(ctx, http.MethodGet, pollURL, nil, &response); err != nil {
			if jobCallback != nil {
				jobCallback(map[string]any{"stage": "failed", "job_id": jobID, "operation": operation, "error": err.Error()})
			}
			return nil, 0, 0, jobID, err
		}
	}
	if len(response.Errors) > 0 {
		message := response.Errors[0].Message
		if jobCallback != nil {
			jobCallback(map[string]any{"stage": "failed", "job_id": jobID, "operation": operation, "error": message})
		}
		return nil, 0, 0, jobID, fmt.Errorf("BigQuery query failed: %s", message)
	}
	processed := parseInt64(response.TotalBytesProcessed)
	billed := parseInt64(response.TotalBytesBilled)
	rows := make([]map[string]any, 0, len(response.Rows))
	appendRows := func(values []queryRow) {
		for _, row := range values {
			item := map[string]any{}
			for i, cell := range row.Values {
				if i < len(response.Schema.Fields) {
					item[response.Schema.Fields[i].Name] = cell.Value
				}
			}
			rows = append(rows, item)
		}
	}
	appendRows(response.Rows)
	page := response.PageToken
	for page != "" {
		pageURL := g.base() + "/projects/" + url.PathEscape(project) + "/queries/" + url.PathEscape(jobID) + "?location=" + url.QueryEscape(locationValue) + "&maxResults=10000&pageToken=" + url.QueryEscape(page)
		var next queryResponse
		if err := g.request(ctx, http.MethodGet, pageURL, nil, &next); err != nil {
			return nil, processed, billed, jobID, err
		}
		if len(next.Schema.Fields) > 0 {
			response.Schema = next.Schema
		}
		appendRows(next.Rows)
		page = next.PageToken
	}
	if processed == 0 || billed == 0 {
		var metadata struct {
			Statistics struct {
				Query struct {
					Processed string `json:"totalBytesProcessed"`
					Billed    string `json:"totalBytesBilled"`
				} `json:"query"`
			} `json:"statistics"`
		}
		jobURL := g.base() + "/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(jobID) + "?location=" + url.QueryEscape(locationValue)
		if err := g.request(ctx, http.MethodGet, jobURL, nil, &metadata); err == nil {
			if processed == 0 {
				processed = parseInt64(metadata.Statistics.Query.Processed)
			}
			if billed == 0 {
				billed = parseInt64(metadata.Statistics.Query.Billed)
			}
		}
	}
	if jobCallback != nil {
		jobCallback(map[string]any{"stage": "completed", "job_id": jobID, "operation": operation, "bytes_processed": processed, "bytes_billed": billed})
	}
	return rows, processed, billed, jobID, nil
}

func (g *googleAPI) cancelJob(project, jobID, location string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	endpoint := g.base() + "/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(jobID) + "/cancel?location=" + url.QueryEscape(location)
	_ = g.request(ctx, http.MethodPost, endpoint, map[string]any{}, nil)
}

func (g *googleAPI) request(ctx context.Context, method, endpoint string, input, output any) error {
	var body *strings.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(data))
	} else {
		body = strings.NewReader("")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := g.http.Do(request)
	if err != nil {
		return fmt.Errorf("Google API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&apiErr)
		message := strings.TrimSpace(apiErr.Error.Message)
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("Google API request failed (%s): %s", firstString(apiErr.Error.Status, response.Status), message)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode Google API response: %w", err)
	}
	return nil
}

func queryParamString(value any) string {
	switch typed := value.(type) {
	case time.Time:
		// BigQuery's TIMESTAMP parameter parser accepts at most microsecond
		// precision. time.Now() carries nanoseconds, which otherwise produces
		// values such as 2026-10-16T13:04:20.814626213Z and is rejected as an
		// unparseable TIMESTAMP.
		return typed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	default:
		return fmt.Sprint(value)
	}
}

func parseInt64(value string) int64 { parsed, _ := strconv.ParseInt(value, 10, 64); return parsed }
