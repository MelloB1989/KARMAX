package safety

import (
	"os"
	"strings"
)

// What a spawned harness inherits.
//
// The old rule stripped ANTHROPIC_* and passed everything else, so KARMAX's API
// token, the WhatsApp webhook secret, the GitLoom key and whatever else lives in
// .env all flowed into every claude and codex subprocess — processes that run
// with permissions skipped and read text strangers wrote. A denylist is the
// wrong shape for that: it fails open on every secret nobody thought of.

// harnessAllowed is what a CLI harness actually needs: where to find its
// binaries, where its own credentials live, and how to render text.
var harnessAllowed = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TERM": true, "TZ": true, "TMPDIR": true, "PWD": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
	"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true,
	"XDG_RUNTIME_DIR": true,
	// Proxy settings, so a harness works on a network that needs one.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	// Certificate paths. Not secrets, and their absence produces TLS errors
	// that look like anything but a missing environment variable.
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
	"NODE_EXTRA_CA_CERTS": true,
}

// HarnessEnv returns the environment a spawned harness should run with.
//
// Extra variables can be let through with KARMAX_HARNESS_ENV_PASSTHROUGH, a
// comma-separated list, so a harness needing something unusual does not require
// a code change — but it is the operator naming it, not a default.
func HarnessEnv() []string {
	allowed := make(map[string]bool, len(harnessAllowed))
	for k := range harnessAllowed {
		allowed[k] = true
	}
	for _, extra := range strings.Split(os.Getenv("KARMAX_HARNESS_ENV_PASSTHROUGH"), ",") {
		if extra = strings.TrimSpace(extra); extra != "" {
			allowed[extra] = true
		}
	}

	env := os.Environ()
	out := make([]string, 0, len(allowed))
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if allowed[key] {
			out = append(out, kv)
		}
	}
	return out
}
