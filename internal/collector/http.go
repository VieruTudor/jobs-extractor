package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

func newHTTPClient(settings map[string]any) *http.Client {
	timeout := numberSetting(settings, "timeout_seconds", 30)
	return &http.Client{Timeout: time.Duration(timeout) * time.Second}
}

func fetchJSON(ctx context.Context, client *http.Client, settings map[string]any, rawURL string) (any, error) {
	retries := numberSetting(settings, "retries", 2)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "PersonalJobCollector/0.1 (Go)")
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 25<<20))
			response.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				var result any
				if err := json.Unmarshal(body, &result); err != nil {
					return nil, fmt.Errorf("decode %s: %w", rawURL, err)
				}
				return result, nil
			} else {
				lastErr = fmt.Errorf("GET %s returned HTTP %d: %s", rawURL, response.StatusCode, truncate(string(body), 300))
				if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
					return nil, lastErr
				}
			}
		} else {
			lastErr = err
		}
		if attempt < retries {
			delay := time.Duration(1<<min(attempt+1, 5)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return nil, lastErr
}

func fetchURL(base string, query url.Values) string {
	if len(query) == 0 {
		return base
	}
	return base + "?" + query.Encode()
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "..."
}
