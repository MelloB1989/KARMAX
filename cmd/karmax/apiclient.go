package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
)

// Small HTTP client for talking to a running KARMAX instance's API. Commands
// use this instead of printing curl hints.

var envOnce sync.Once

func ensureEnv() { envOnce.Do(loadDotEnv) }

func apiBaseURL() string {
	ensureEnv()
	if v := strings.TrimSpace(os.Getenv("KARMAX_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	port := 9091
	if cfg, err := config.Load(findConfig()); err == nil && cfg.API.Port != 0 {
		port = cfg.API.Port
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

func apiToken() string {
	ensureEnv()
	return strings.TrimSpace(os.Getenv("KARMAX_API_TOKEN"))
}

func apiGET(path string) (map[string]any, error)  { return apiDo(http.MethodGet, path) }
func apiPOST(path string) (map[string]any, error) { return apiDo(http.MethodPost, path) }

func apiDo(method, path string) (map[string]any, error) {
	base := apiBaseURL()
	req, err := http.NewRequest(method, base+path, nil)
	if err != nil {
		return nil, err
	}
	if t := apiToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("KARMAX API unreachable at %s — is it running? (start it with `karmax start`)", base)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode >= 300 {
		if msg, ok := out["error"].(string); ok && msg != "" {
			return out, fmt.Errorf("%s", msg)
		}
		return out, fmt.Errorf("API %s returned %s", path, resp.Status)
	}
	return out, nil
}

// asList coerces a JSON field to a slice of objects.
func asList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
