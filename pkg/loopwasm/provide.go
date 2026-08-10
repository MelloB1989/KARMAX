package loopwasm

import (
	"encoding/json"
	"fmt"
)

// Providing tools to the agent.
//
// A loop declares in its manifest what it provides:
//
//	provides:
//	  - name: deal.status
//	    description: Where a named deal stands right now
//	    parameters:
//	      type: object
//	      properties:
//	        deal: {type: string, description: The deal to look up}
//	      required: [deal]
//
// and registers the implementation in init, so it is in place before the host
// calls it — the tool entry point runs on a fresh instance where the loop's
// run() has not executed:
//
//	func init() {
//	    loopwasm.Provide("deal.status", func(in struct{ Deal string }) (any, error) {
//	        return recallDeal(in.Deal)
//	    })
//	}
//
// The agent is given these tools only while it is working on this loop's
// behalf: during an ask() the loop made, and during a later turn caused by it.
// They are not part of the agent's permanent toolset.

// Handler serves one call to a provided tool. It receives the raw JSON input
// and returns whatever should be handed back to the model.
type Handler func(input json.RawMessage) ([]byte, error)

var handlers = map[string]Handler{}

// ProvideRaw registers a handler that takes and returns raw JSON. Use Provide
// unless the input shape is genuinely dynamic.
func ProvideRaw(name string, h Handler) { handlers[name] = h }

// Provide registers a typed handler for a provided tool.
//
// The input is decoded into T and the result marshalled to JSON, so a handler
// deals in Go values rather than in bytes. A string result is passed through
// unquoted, since a model reads it as text either way.
func Provide[T any, R any](name string, h func(T) (R, error)) {
	handlers[name] = func(raw json.RawMessage) ([]byte, error) {
		var in T
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("input did not match what %s expects: %w", name, err)
			}
		}
		out, err := h(in)
		if err != nil {
			return nil, err
		}
		if s, ok := any(out).(string); ok {
			return []byte(s), nil
		}
		return json.Marshal(out)
	}
}

// Provided lists the tools registered in this module, for tests.
func Provided() []string {
	out := make([]string, 0, len(handlers))
	for name := range handlers {
		out = append(out, name)
	}
	return out
}

// Serve runs a registered handler directly. Off-target this is the only way to
// reach one, so a loop's tools can be unit tested without a wasm toolchain.
func Serve(name string, input json.RawMessage) ([]byte, error) {
	h, ok := handlers[name]
	if !ok {
		return nil, fmt.Errorf("loopwasm: no tool named %q is registered", name)
	}
	return h(input)
}

func errorResult(msg string) []byte {
	b, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		return []byte(`{"error":"the error could not be encoded"}`)
	}
	return b
}
