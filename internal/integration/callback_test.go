package integration

import (
	"fmt"
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
