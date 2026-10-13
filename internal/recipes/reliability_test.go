package recipes

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// memoKit checkpoints like the real runtime: a step's result is stored on
// success only, and a later Run against the same store skips stored steps.
type memoKit struct {
	*DryRun
	store map[string]string
	ran   map[string]int // executions per step id
	http  func(call int) (string, int, http.Header, error)
	calls int
}

func newMemoKit(store map[string]string) *memoKit {
	return &memoKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual}),
		store: store, ran: map[string]int{}}
}

func (k *memoKit) Step(name string, fn func() (string, error)) (string, error) {
	if v, ok := k.store[name]; ok {
		return v, nil
	}
	k.ran[name]++
	v, err := fn()
	if err != nil {
		return "", err
	}
	k.store[name] = v
	return v, nil
}

func (k *memoKit) Once(name string, fn func() error) error {
	_, err := k.Step(name, func() (string, error) { return "", fn() })
	return err
}

func (k *memoKit) Logf(string, ...any) {}

func (k *memoKit) Ask(_ context.Context, p string) (string, error) {
	if strings.Contains(p, "boom") {
		return "", errors.New("ask exploded")
	}
	return "answered", nil
}

func (k *memoKit) HTTPWithHeaders(_ context.Context, _, _ string, _ map[string]string, _ string) (string, int, http.Header, error) {
	k.calls++
	return k.http(k.calls)
}

func (k *memoKit) HTTP(ctx context.Context, m, u string, h map[string]string, b string) (string, int, error) {
	o, s, _, e := k.HTTPWithHeaders(ctx, m, u, h, b)
	return o, s, e
}

func noSleep(t *testing.T) *[]time.Duration {
	var waits []time.Duration
	old := retrySleep
	retrySleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { retrySleep = old })
	return &waits
}

// ---- on_error: continue ----

const onErrorRecipe = `
name: x
on:
  manual: true
steps:
  - ask: "boom"
    as: a
    on_error: continue
  - ask: "after {{ .a_error }}"
    as: b
`

func TestOnErrorContinueCapturesAndGoes(t *testing.T) {
	r := mustParse(t, onErrorRecipe)
	k := newMemoKit(map[string]string{})
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatalf("run must not fail: %v", err)
	}
	if got := k.store["0-ask-onerror"]; got != "ask exploded" {
		t.Fatalf("captured error not checkpointed: %v", k.store)
	}
	if k.store["1-ask"] != "answered" {
		t.Fatalf("later step did not run: %v", k.store)
	}
}

func TestOnErrorBindsEmptyValueAndMessage(t *testing.T) {
	b := Bindings{}
	bindCaptured(b, Step{As: "x", OnError: OnErrorContinue}, "bad")
	if b["x"] != "" || b["x_error"] != "bad" {
		t.Fatalf("bindings = %v", b)
	}
}

func TestOnErrorSuccessSetsEmptyErrorBinding(t *testing.T) {
	r := mustParse(t, strings.Replace(onErrorRecipe, `"boom"`, `"fine"`, 1))
	k := newMemoKit(map[string]string{})
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.store["0-ask-onerror"]; ok {
		t.Fatal("no capture should be recorded on success")
	}
}

func TestWithoutOnErrorAFailureFailsTheRun(t *testing.T) {
	r := mustParse(t, strings.Replace(onErrorRecipe, "    on_error: continue\n", "", 1))
	if err := Run(context.Background(), r, newMemoKit(map[string]string{})); err == nil {
		t.Fatal("expected the run to fail")
	}
}

// A resumed run must not re-execute a step whose failure was already captured
// and acted on — even if it would now succeed.
func TestResumedRunDoesNotRerunACapturedFailure(t *testing.T) {
	store := map[string]string{}
	r := mustParse(t, onErrorRecipe+"  - ask: \"boom again\"\n")
	k1 := newMemoKit(store)
	if err := Run(context.Background(), r, k1); err == nil {
		t.Fatal("third step has no on_error, run should fail")
	}
	k2 := newMemoKit(store)
	_ = Run(context.Background(), r, k2)
	if k2.ran["0-ask"] != 0 {
		t.Fatalf("captured step re-executed on resume: %v", k2.ran)
	}
	if k2.ran["2-ask"] != 1 {
		t.Fatalf("the failed uncaptured step must be retried: %v", k2.ran)
	}
}

func TestOnErrorDoesNotSwallowSuspensionOrCancellation(t *testing.T) {
	if capturable(context.Background(), loopkit.ErrSuspended) {
		t.Fatal("suspension is not a failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if capturable(ctx, errors.New("x")) {
		t.Fatal("a cancelled run must not be swallowed")
	}
}

func TestOnErrorValidation(t *testing.T) {
	for _, bad := range []string{"ignore", "retry", "true"} {
		_, err := parse(t, strings.Replace(onErrorRecipe, "continue", bad, 1))
		if err == nil || !strings.Contains(err.Error(), "on_error") {
			t.Errorf("on_error: %s should be rejected, got %v", bad, err)
		}
	}
	if _, err := parse(t, `
name: x
on: {manual: true}
steps:
  - await: {event: e}
    on_error: continue
`); err == nil {
		t.Error("on_error on await should be rejected")
	}
}

// ---- http retry ----

func httpRecipe(retry string) string {
	return `
name: x
on:
  manual: true
steps:
  - http:
      url: https://example.com/x
      method: POST
` + retry + `
    as: r
`
}

func TestHTTPRetriesOn5xxThenSucceeds(t *testing.T) {
	waits := noSleep(t)
	r := mustParse(t, httpRecipe("      retry: { attempts: 4, backoff: 2s }"))
	k := newMemoKit(map[string]string{})
	k.http = func(n int) (string, int, http.Header, error) {
		if n < 3 {
			return "", 503, nil, nil
		}
		return "ok", 200, nil, nil
	}
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if k.calls != 3 {
		t.Fatalf("calls = %d, want 3", k.calls)
	}
	if len(*waits) != 2 || (*waits)[0] != 2*time.Second || (*waits)[1] != 4*time.Second {
		t.Fatalf("waits = %v, want exponential 2s,4s", *waits)
	}
}

func TestHTTPDoesNotRetryOther4xx(t *testing.T) {
	noSleep(t)
	r := mustParse(t, httpRecipe("      retry: { attempts: 5 }"))
	k := newMemoKit(map[string]string{})
	k.http = func(int) (string, int, http.Header, error) { return "", 409, nil, nil }
	if err := Run(context.Background(), r, k); err == nil {
		t.Fatal("expected failure")
	}
	if k.calls != 1 {
		t.Fatalf("a 409 must not be retried, calls = %d", k.calls)
	}
}

func TestHTTPRetries429HonouringRetryAfterAndCapsIt(t *testing.T) {
	waits := noSleep(t)
	r := mustParse(t, httpRecipe("      retry: { attempts: 3, backoff: 1s }"))
	k := newMemoKit(map[string]string{})
	k.http = func(n int) (string, int, http.Header, error) {
		switch n {
		case 1:
			return "", 429, http.Header{"Retry-After": {"7"}}, nil
		case 2:
			return "", 429, http.Header{"Retry-After": {"3600"}}, nil
		}
		return "ok", 200, nil, nil
	}
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if (*waits)[0] != 7*time.Second || (*waits)[1] != maxRetryWait {
		t.Fatalf("waits = %v, want 7s then the %s cap", *waits, maxRetryWait)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestHTTPRetriesNetworkErrorsButNotRefusals(t *testing.T) {
	noSleep(t)
	r := mustParse(t, httpRecipe("      retry: { attempts: 3 }"))

	k := newMemoKit(map[string]string{})
	k.http = func(int) (string, int, http.Header, error) { return "", 0, nil, timeoutErr{} }
	if err := Run(context.Background(), r, k); err == nil || k.calls != 3 {
		t.Fatalf("network errors should use all attempts: calls=%d err=%v", k.calls, err)
	}

	k = newMemoKit(map[string]string{})
	k.http = func(int) (string, int, http.Header, error) { return "", 0, nil, errors.New("host not granted") }
	if err := Run(context.Background(), r, k); err == nil || k.calls != 1 {
		t.Fatalf("a refusal must not be retried: calls=%d err=%v", k.calls, err)
	}
}

func TestHTTPRetryStopsAtTheContextDeadline(t *testing.T) {
	waits := noSleep(t)
	r := mustParse(t, httpRecipe("      retry: { attempts: 5, backoff: 30s }"))
	k := newMemoKit(map[string]string{})
	k.http = func(int) (string, int, http.Header, error) { return "", 500, nil, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, r, k)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want the real 500 error, got %v", err)
	}
	if len(*waits) != 0 || k.calls != 1 {
		t.Fatalf("must not sleep past the deadline: waits=%v calls=%d", *waits, k.calls)
	}
}

func TestHTTPWithoutRetryIsSingleShot(t *testing.T) {
	r := mustParse(t, httpRecipe(""))
	k := newMemoKit(map[string]string{})
	k.http = func(int) (string, int, http.Header, error) { return "", 502, nil, nil }
	if err := Run(context.Background(), r, k); err == nil || k.calls != 1 {
		t.Fatalf("calls=%d err=%v", k.calls, err)
	}
}

func TestHTTPRetryValidation(t *testing.T) {
	for _, bad := range []string{
		"retry: { attempts: 0 }", "retry: { attempts: x }", "retry: { attempts: 99 }",
		"retry: { attempts: 3, backoff: soon }", "retry: { backoff: 2s }", "retry: { attempts: 3, jitter: 1 }",
	} {
		if _, err := parse(t, httpRecipe("      "+bad)); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d := parseRetryAfter("12", now); d != 12*time.Second {
		t.Error(d)
	}
	if d := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); d != 30*time.Second {
		t.Error(d)
	}
	if parseRetryAfter("junk", now) != 0 || parseRetryAfter("-3", now) != 0 {
		t.Error("junk and negatives are ignored")
	}
}

// ---- foreach limit ----

func TestForeachLimit(t *testing.T) {
	r := mustParse(t, `
name: x
on:
  manual: true
steps:
  - foreach:
      in: '["a","b","c","d"]'
      as: t
      limit: 2
      steps:
        - ask: "go {{ .t }}"
`)
	k := newMemoKit(map[string]string{})
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if len(k.store) != 2 {
		t.Fatalf("ran %d iterations, want 2: %v", len(k.store), k.store)
	}
}

func TestForeachLimitLargerThanListAndValidation(t *testing.T) {
	r := mustParse(t, `
name: x
on: {manual: true}
steps:
  - foreach:
      in: ["a","b"]
      as: t
      limit: 10
      steps:
        - ask: "go {{ .t }}"
`)
	k := newMemoKit(map[string]string{})
	if err := Run(context.Background(), r, k); err != nil || len(k.store) != 2 {
		t.Fatalf("err=%v store=%v", err, k.store)
	}
	for _, bad := range []string{"0", "-1", "two"} {
		_, err := parse(t, strings.Replace(`
name: x
on: {manual: true}
steps:
  - foreach:
      in: ["a"]
      as: t
      limit: N
      steps:
        - log: hi
`, "N", bad, 1))
		if err == nil {
			t.Errorf("limit %s should be rejected", bad)
		}
	}
}

// ---- harness max_turns ----

func TestHarnessMaxTurnsReachesTheSpecEvenWithoutASession(t *testing.T) {
	r := mustParse(t, `
name: x
on: {manual: true}
steps:
  - harness:
      text: "work"
      max_turns: 12
    as: out
`)
	k := &harnessRecordingKit{DryRun: NewDryRun(loopkit.Trigger{Kind: loopkit.TriggerManual})}
	if err := Run(context.Background(), r, k); err != nil {
		t.Fatal(err)
	}
	if len(k.specs) != 1 || k.specs[0].MaxTurns != 12 || !k.specs[0].Ephemeral {
		t.Fatalf("specs = %+v", k.specs)
	}
}

func TestHarnessMaxTurnsValidation(t *testing.T) {
	for _, bad := range []string{"0", "-2", "many", "1.5"} {
		_, err := parse(t, "name: x\non: {manual: true}\nsteps:\n  - harness:\n      text: hi\n      max_turns: "+bad+"\n")
		if err == nil {
			t.Errorf("max_turns %s should be rejected", bad)
		}
	}
	if _, err := parse(t, "name: x\non: {manual: true}\nsteps:\n  - ask:\n      text: hi\n      max_turns: 3\n"); err == nil {
		t.Error("max_turns on ask should be rejected")
	}
}
