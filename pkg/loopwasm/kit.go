// Package loopwasm is the SDK for writing a KARMAX loop as a WASM module.
//
// A loop built with this runs sandboxed: no filesystem, no environment, no
// sockets, and no host function it did not declare in its manifest. That is the
// point — it means someone can install your loop without reading it first.
//
// Build one with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o loop.wasm .
//
// then sign it with `karmax loops sign`.
//
//	//go:wasmexport run
//	func run() {
//	    hits, _ := loopwasm.Recall("what did we agree with the vendor", 5)
//	    loopwasm.Notify("Vendor", strings.Join(hits, "\n"))
//	}
//
// The package compiles on a normal machine too, where every call returns
// ErrNotInWASM — so the logic can be unit tested without a wasm toolchain.
package loopwasm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotInWASM is returned when a host call is attempted outside KARMAX.
var ErrNotInWASM = errors.New("loopwasm: not running inside KARMAX")

// Errors the host returns. A refusal is distinguishable from a failure, so a
// loop can say "I was not allowed to do that" rather than "it did not work".
var (
	// ErrNotDeclared means the manifest did not ask for this host function.
	ErrNotDeclared = errors.New("loopwasm: this loop did not declare that capability in its manifest")
	// ErrNotPermitted means the operator has not granted it.
	ErrNotPermitted = errors.New("loopwasm: the operator has not granted that capability")
	// ErrBufferTooSmall means the response did not fit; retry with more room.
	ErrBufferTooSmall = errors.New("loopwasm: the response did not fit in the buffer")
)

const (
	fnLog      = "log"
	fnRecall   = "recall"
	fnRemember = "remember"
	fnNotify   = "notify"
	fnHTTP     = "http"
	fnAsk      = "ask"
)

// initialBuffer is where a response is read into. Grown and retried when the
// host says it did not fit, so a large answer is not an error.
const initialBuffer = 32 << 10

// Log writes a line to KARMAX's log, attributed to this loop.
func Log(format string, args ...any) {
	_, _ = request(fnLog, fmt.Sprintf(format, args...))
}

// Recall returns memories matching a query.
func Recall(query string, limit int) ([]string, error) {
	req, err := json.Marshal(map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	out, err := request(fnRecall, string(req))
	if err != nil {
		return nil, err
	}
	var res struct {
		Hits []string `json:"hits"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// Remember stores a durable fact.
func Remember(fact string) error {
	req, err := json.Marshal(map[string]any{"fact": fact})
	if err != nil {
		return err
	}
	_, err = request(fnRemember, string(req))
	return err
}

// Notify sends the operator a notification.
func Notify(title, body string) error {
	req, err := json.Marshal(map[string]any{"title": title, "body": body})
	if err != nil {
		return err
	}
	_, err = request(fnNotify, string(req))
	return err
}

// Ask puts a question to the operator's agent, which has tools and judgement.
func Ask(prompt string) (string, error) {
	req, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		return "", err
	}
	out, err := request(fnAsk, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Answer, nil
}

// Response is the result of an HTTP call.
type Response struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// HTTP makes a request, which must be to a host the manifest declared.
func HTTP(method, url string, headers map[string]string, body string) (*Response, error) {
	req, err := json.Marshal(map[string]any{
		"method": method, "url": url, "headers": headers, "body": body,
	})
	if err != nil {
		return nil, err
	}
	out, err := request(fnHTTP, string(req))
	if err != nil {
		return nil, err
	}
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// request performs one host call, growing the buffer if the answer did not fit.
func request(name, payload string) ([]byte, error) {
	size := initialBuffer
	for attempt := 0; attempt < 4; attempt++ {
		out := make([]byte, size)
		n := hostCall(name, payload, out)
		switch {
		case n >= 0:
			return out[:n], nil
		case n == -2:
			return nil, ErrNotDeclared
		case n == -3:
			return nil, ErrNotPermitted
		case n == -4:
			size *= 8
			continue
		case n == -5:
			return nil, fmt.Errorf("loopwasm: %s rejected the request as malformed", name)
		default:
			return nil, fmt.Errorf("loopwasm: %s failed", name)
		}
	}
	return nil, ErrBufferTooSmall
}
