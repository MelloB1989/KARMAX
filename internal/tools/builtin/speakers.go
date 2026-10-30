package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// SpeakerLabeller adds who-wrote-it to one message object (anything carrying a
// "sender_jid"), in place.
type SpeakerLabeller func(ctx context.Context, msg map[string]any)

// AttributeSpeakers labels every message in what the given tools return with
// who wrote it.
//
// wacli reports a sender id and is_from_me per message and nothing else about
// who anybody is. A model reading a thread had to work out from bare ids which
// line was the operator's — and "from me" covers both KARMAX's sends and the
// operator typing on the same account. Apply after GuardUntrusted: the guard
// recognises the tools it wraps by type, and the labels are KARMAX's own words,
// which need no defanging.
func AttributeSpeakers(in []tools.Tool, label SpeakerLabeller) []tools.Tool {
	if label == nil {
		return in
	}
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		out = append(out, &speakerTool{inner: t, label: label})
	}
	return out
}

type speakerTool struct {
	inner tools.Tool
	label SpeakerLabeller
}

func (s *speakerTool) Manifest() tools.ToolManifest { return s.inner.Manifest() }

func (s *speakerTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	res, err := s.inner.Execute(ctx, input)
	if err != nil || res.IsError {
		return res, err
	}
	m, ok := res.Output.(map[string]any)
	if !ok {
		return res, nil
	}
	raw, ok := m["raw"].(string)
	if !ok || !strings.Contains(raw, `"sender_jid"`) {
		return res, nil
	}
	if labelled, changed := labelSpeakers(ctx, raw, s.label); changed {
		m["raw"] = labelled
	}
	return res, nil
}

// labelSpeakers rewrites a JSON document with every object that has a
// "sender_jid" labelled. changed is false when the text is not JSON or holds
// no sender, and the caller then keeps the original byte for byte.
func labelSpeakers(ctx context.Context, raw string, label SpeakerLabeller) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber() // ids and sizes come back exactly as they went in
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw, false
	}
	n := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["sender_jid"].(string); ok {
				label(ctx, x)
				n++
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	if n == 0 {
		return raw, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return raw, false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}
