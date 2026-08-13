package integration

import (
	"fmt"
	"net"
	"testing"
)

func TestCallbackAddressIsFixedByDefault(t *testing.T) {
	t.Setenv("KARMAX_OAUTH_CALLBACK_HOST", "")
	t.Setenv("KARMAX_OAUTH_CALLBACK_PORT", "")

	// Fixed is the whole point: an OS-assigned port cannot be registered with a
	// provider that matches redirect URLs exactly.
	h1, p1 := callbackAddress()
	h2, p2 := callbackAddress()
	if h1 != h2 || p1 != p2 {
		t.Fatalf("callback moved between calls: %s:%d then %s:%d", h1, p1, h2, p2)
	}
	if p1 != defaultCallbackPort || h1 != defaultCallbackHost {
		t.Errorf("got %s:%d, want %s:%d", h1, p1, defaultCallbackHost, defaultCallbackPort)
	}
}

func TestCallbackAddressIsOverridable(t *testing.T) {
	t.Setenv("KARMAX_OAUTH_CALLBACK_HOST", "localhost")
	t.Setenv("KARMAX_OAUTH_CALLBACK_PORT", "9200")
	h, p := callbackAddress()
	if h != "localhost" || p != 9200 {
		t.Errorf("got %s:%d, want localhost:9200", h, p)
	}
}

func TestCallbackAddressIgnoresNonsensePorts(t *testing.T) {
	// A bad value must not silently become port 0 — that is the ephemeral
	// behaviour this replaced, and it would fail with a redirect mismatch the
	// operator cannot diagnose.
	for _, bad := range []string{"nope", "0", "-1", "70000"} {
		t.Setenv("KARMAX_OAUTH_CALLBACK_PORT", bad)
		if _, p := callbackAddress(); p != defaultCallbackPort {
			t.Errorf("port %q gave %d, want the default %d", bad, p, defaultCallbackPort)
		}
	}
}

func TestCallbackURLIsStable(t *testing.T) {
	t.Setenv("KARMAX_OAUTH_CALLBACK_HOST", "")
	t.Setenv("KARMAX_OAUTH_CALLBACK_PORT", "")
	h, p := callbackAddress()
	if got := fmt.Sprintf("http://%s:%d/callback", h, p); got != "http://127.0.0.1:9095/callback" {
		t.Errorf("callback URL = %s", got)
	}
}

func TestListenLoopbackAnswersBothFamiliesForAHostname(t *testing.T) {
	// A browser resolving "localhost" may reach for ::1 while Go's own
	// net.Listen("localhost:port") binds only 127.0.0.1 — the callback then
	// hangs until the flow times out, with nothing naming the cause.
	ls, err := listenLoopback("localhost", 19097)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()

	reached := 0
	for _, addr := range []string{"127.0.0.1:19097", "[::1]:19097"} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Logf("%s not reachable (%v) — acceptable only if that family is absent", addr, err)
			continue
		}
		reached++
		c.Close()
	}
	if reached == 0 {
		t.Error("a hostname callback must be reachable on at least one loopback family")
	}
	if reached < len(ls) {
		t.Errorf("bound %d listeners but only %d were reachable", len(ls), reached)
	}
}

func TestListenLoopbackTakesALiteralIPAtItsWord(t *testing.T) {
	ls, err := listenLoopback("127.0.0.1", 19098)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	if len(ls) != 1 {
		t.Fatalf("an explicit IP should bind exactly one address, got %d", len(ls))
	}
	if got := ls[0].Addr().String(); got != "127.0.0.1:19098" {
		t.Errorf("bound %s", got)
	}
}

func TestListenLoopbackReportsAPortAlreadyTaken(t *testing.T) {
	first, err := listenLoopback("127.0.0.1", 19099)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer first[0].Close()
	// Must fail rather than quietly land somewhere else: a moved port is a
	// redirect_uri mismatch the operator cannot diagnose from the browser.
	if _, err := listenLoopback("127.0.0.1", 19099); err == nil {
		t.Error("expected an error when the port is already held")
	}
}
