package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KarmaxTool is one tool KARMAX's API offers (GET /api/tools).
type KarmaxTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// KarmaxResult is what one tool call returned: its output, or the error the
// tool itself reported (OK false).
type KarmaxResult struct {
	Tool   string          `json:"tool"`
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Tools lists KARMAX's tools, as its default agent runs them.
func (k Karmax) Tools(ctx context.Context) ([]KarmaxTool, error) {
	var out struct {
		Tools []KarmaxTool `json:"tools"`
	}
	if err := k.api(ctx, http.MethodGet, "/api/tools", nil, &out, 30*time.Second); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Call runs one KARMAX tool with KARMAX's token, which never leaves fleetd.
// Nothing but the name and the arguments is passed on: no query (?agent=,
// ?as=, ?turn=) a caller could use to reach further.
func (k Karmax) Call(ctx context.Context, name string, args map[string]any) (KarmaxResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var out KarmaxResult
	err := k.api(ctx, http.MethodPost, "/api/tools/"+url.PathEscape(name), args, &out, 12*time.Minute)
	if out.Tool == "" {
		out.Tool = name
	}
	return out, err
}

func (k Karmax) api(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.APIURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+k.APIToken)
	}
	c := k.Client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("KARMAX unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("KARMAX: %s", e.Error)
		}
		return fmt.Errorf("KARMAX: %s", resp.Status)
	}
	return json.Unmarshal(b, out)
}
