package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Param is one declared parameter of a tool, flattened from its JSON schema.
type Param struct {
	Name        string
	Type        string
	Required    bool
	Enum        []string
	Description string
}

// ParamSpec is a tool's declared parameters. Checked is false when the schema
// declares no properties (or none we can read): such a tool is loose and its
// input is never validated.
type ParamSpec struct {
	Params  []Param
	Checked bool
	// Extra is true when additionalProperties is true, so unknown keys pass
	// but missing required keys are still an error.
	Extra bool
}

// ParseParams reads a tool's JSON-schema parameters. Required parameters come
// first, then the rest alphabetically.
func ParseParams(raw json.RawMessage) ParamSpec {
	var s struct {
		Properties map[string]struct {
			Type        any    `json:"type"`
			Description string `json:"description"`
			Enum        []any  `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties any      `json:"additionalProperties"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ParamSpec{}
	}
	spec := ParamSpec{}
	if b, ok := s.AdditionalProperties.(bool); ok && b {
		spec.Extra = true
	}
	if len(s.Properties) == 0 {
		return spec
	}
	spec.Checked = true
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	for name, p := range s.Properties {
		pr := Param{Name: name, Required: req[name], Description: p.Description}
		switch t := p.Type.(type) {
		case string:
			pr.Type = t
		case []any:
			var ts []string
			for _, x := range t {
				ts = append(ts, fmt.Sprint(x))
			}
			pr.Type = strings.Join(ts, "|")
		}
		for _, e := range p.Enum {
			pr.Enum = append(pr.Enum, fmt.Sprint(e))
		}
		spec.Params = append(spec.Params, pr)
	}
	// A required key naming no property is still required.
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			spec.Params = append(spec.Params, Param{Name: r, Required: true})
		}
	}
	sort.Slice(spec.Params, func(i, j int) bool {
		a, b := spec.Params[i], spec.Params[j]
		if a.Required != b.Required {
			return a.Required
		}
		return a.Name < b.Name
	})
	return spec
}

// Summary renders the parameters on one line, e.g.
// `title (required, string), detail, severity (info|warning|critical)`.
func (s ParamSpec) Summary() string {
	parts := make([]string, 0, len(s.Params))
	for _, p := range s.Params {
		var notes []string
		if p.Required {
			notes = append(notes, "required")
		}
		switch {
		case len(p.Enum) > 0:
			notes = append(notes, strings.Join(p.Enum, "|"))
		case p.Type != "" && p.Required:
			notes = append(notes, p.Type)
		}
		if p.Description != "" && len(p.Enum) == 0 {
			notes = append(notes, p.Description)
		}
		if len(notes) == 0 {
			parts = append(parts, p.Name)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Name, strings.Join(notes, ", ")))
	}
	return strings.Join(parts, ", ")
}

// ValidateInput checks input against a tool's declared parameters: a key the
// schema does not declare, or a missing required key, is an error that names
// what the tool does take. A tool with no declared properties is unchecked, and
// additionalProperties:true lets unknown keys through.
func ValidateInput(m ToolManifest, input map[string]any) error {
	spec := ParseParams(m.Parameters)
	if !spec.Checked {
		return nil
	}
	known := make(map[string]bool, len(spec.Params))
	for _, p := range spec.Params {
		known[p.Name] = true
	}
	if !spec.Extra {
		var unknown []string
		for k := range input {
			if !known[k] {
				unknown = append(unknown, fmt.Sprintf("%q", k))
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			word := "argument"
			if len(unknown) > 1 {
				word = "arguments"
			}
			return fmt.Errorf("%s: unknown %s %s; takes %s", m.Name, word, strings.Join(unknown, ", "), spec.Summary())
		}
	}
	var missing []string
	for _, p := range spec.Params {
		if !p.Required {
			continue
		}
		if v, ok := input[p.Name]; !ok || v == nil {
			missing = append(missing, fmt.Sprintf("%q", p.Name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: missing required argument %s; takes %s", m.Name, strings.Join(missing, ", "), spec.Summary())
	}
	return nil
}
