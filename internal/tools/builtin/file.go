package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MelloB1989/karmax/internal/tools"
)

type FileReadTool struct{}

func (t *FileReadTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "file.read",
		Description: "Read file content from the filesystem",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"},
				"encoding": {"type": "string", "default": "utf-8"}
			},
			"required": ["path"]
		}`),
	}
}

func (t *FileReadTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	path, _ := input["path"].(string)
	if path == "" {
		return tools.ErrorResult(fmt.Errorf("path is required")), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	return tools.SuccessResult(map[string]any{
		"content": string(data),
		"size":    len(data),
	}), nil
}

type FileWriteTool struct{}

func (t *FileWriteTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "file.write",
		Description: "Write or append content to a file",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"},
				"content": {"type": "string"},
				"mode": {"type": "string", "enum": ["write", "append"], "default": "write"}
			},
			"required": ["path", "content"]
		}`),
	}
}

func (t *FileWriteTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	path, _ := input["path"].(string)
	content, _ := input["content"].(string)
	if path == "" {
		return tools.ErrorResult(fmt.Errorf("path is required")), nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return tools.ErrorResult(err), nil
	}

	mode, _ := input["mode"].(string)
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if mode == "append" {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}

	f, err := os.OpenFile(path, flag, 0644)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	defer f.Close()

	n, err := f.WriteString(content)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	return tools.SuccessResult(map[string]any{
		"bytes_written": n,
		"path":          path,
	}), nil
}

type FileListTool struct{}

func (t *FileListTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "file.list",
		Description: "List files and directories in a path",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"},
				"pattern": {"type": "string"}
			},
			"required": ["path"]
		}`),
	}
}

func (t *FileListTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	path, _ := input["path"].(string)
	if path == "" {
		return tools.ErrorResult(fmt.Errorf("path is required")), nil
	}

	pattern, _ := input["pattern"].(string)

	entries, err := os.ReadDir(path)
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	type fileInfo struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}

	var files []fileInfo
	for _, e := range entries {
		if pattern != "" {
			matched, _ := filepath.Match(pattern, e.Name())
			if !matched {
				continue
			}
		}
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		files = append(files, fileInfo{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}

	return tools.SuccessResult(files), nil
}
