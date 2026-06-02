package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

type HTTPTool struct{}

func (t *HTTPTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "http.request",
		Description: "Make HTTP requests to external URLs",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"method": {"type": "string", "enum": ["GET","POST","PUT","PATCH","DELETE","HEAD"]},
				"url": {"type": "string"},
				"headers": {"type": "object", "additionalProperties": {"type": "string"}},
				"body": {"type": "string"},
				"timeout_ms": {"type": "integer"}
			},
			"required": ["method", "url"]
		}`),
	}
}

func (t *HTTPTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	method, _ := input["method"].(string)
	url, _ := input["url"].(string)
	if method == "" || url == "" {
		return tools.ErrorResult(fmt.Errorf("method and url are required")), nil
	}

	timeout := 30000
	if ms, ok := input["timeout_ms"].(float64); ok {
		timeout = int(ms)
	}

	var bodyReader io.Reader
	if body, ok := input["body"].(string); ok && body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	if headers, ok := input["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				req.Header.Set(k, s)
			}
		}
	}

	client := &http.Client{Timeout: time.Duration(timeout) * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	return tools.SuccessResult(map[string]any{
		"status":  resp.StatusCode,
		"headers": flattenHeaders(resp.Header),
		"body":    string(respBody),
	}), nil
}

func flattenHeaders(h http.Header) map[string]string {
	flat := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			flat[k] = v[0]
		}
	}
	return flat
}
