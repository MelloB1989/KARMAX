package googleacct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner runs one CLI command and returns what it wrote and how it exited.
// code is the exit status; err is non-nil when the command failed to run or
// exited non-zero.
type Runner func(ctx context.Context, env []string, bin string, args ...string) (stdout, stderr []byte, code int, err error)

// runTimeout bounds one CLI call: a hung CLI must not hold a tick forever.
const runTimeout = 90 * time.Second

// ExecRunner runs commands on this host.
func ExecRunner(ctx context.Context, env []string, bin string, args ...string) ([]byte, []byte, int, error) {
	cctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return out.Bytes(), errOut.Bytes(), code, err
}

// cliError is one failed CLI call, already classified.
type cliError struct {
	path   Path
	op     string
	code   int
	detail string
	// auth means the login itself is the problem — signed out, revoked,
	// missing a scope — so another login to the same account may still work.
	auth bool
	// gone means Google answered 404/410 for the thing asked about.
	gone bool
}

func (e *cliError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.path, e.op, e.detail)
}

var (
	accessTokenRE  = regexp.MustCompile(`ya29\.[A-Za-z0-9._\-]+`)
	refreshTokenRE = regexp.MustCompile(`1//[A-Za-z0-9._\-]+`)
	secretFieldRE  = regexp.MustCompile(`"(access_token|refresh_token|client_secret|id_token)"\s*:\s*"[^"]*"`)
)

// redact keeps a CLI's complaint readable and free of anything secret.
func redact(s string) string {
	s = accessTokenRE.ReplaceAllString(s, "<token>")
	s = refreshTokenRE.ReplaceAllString(s, "<token>")
	s = secretFieldRE.ReplaceAllString(s, `"$1":"<redacted>"`)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 400 {
		s = string(r[:400]) + "…"
	}
	return s
}

// classify turns a failed call into a cliError a caller can act on.
//
// gog documents its exit codes (4 is auth_required); gws exits 2 on an auth
// error. Both also report a revoked refresh token as a plain failure whose
// text says invalid_grant, so the text is read as well.
func classify(path Path, op string, code int, stdout, stderr []byte, runErr error) *cliError {
	detail := strings.TrimSpace(string(stderr))
	if body := strings.TrimSpace(string(stdout)); body != "" && (detail == "" || strings.Contains(body, `"error"`)) {
		detail = strings.TrimSpace(detail + " " + body)
	}
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}
	e := &cliError{path: path, op: op, code: code, detail: redact(detail)}
	low := strings.ToLower(detail)
	switch {
	case path == Gog && code == 4, path == Gws && code == 2:
		e.auth = true
	case strings.Contains(low, "invalid_grant"), strings.Contains(low, "unauthenticated"),
		strings.Contains(low, "expired or revoked"), strings.Contains(low, "insufficient authentication scopes"),
		strings.Contains(low, "access_token_scope_insufficient"),
		strings.Contains(low, `"code": 401`), strings.Contains(low, `"code":401`):
		e.auth = true
	}
	if path == Gog && code == 5 || strings.Contains(low, `"code": 404`) || strings.Contains(low, `"code":404`) ||
		strings.Contains(low, `"code": 410`) || strings.Contains(low, `"code":410`) || strings.Contains(low, "fullsyncrequired") ||
		strings.Contains(low, "sync token is no longer valid") {
		e.gone = true
	}
	return e
}

// decodeJSON reads a CLI's JSON output, skipping any banner line before it.
func decodeJSON(raw []byte, out any) error {
	i := bytes.IndexAny(raw, "{[")
	if i < 0 {
		return fmt.Errorf("no JSON in the output")
	}
	return json.Unmarshal(raw[i:], out)
}

// jsonArg renders a --params or --json argument.
func jsonArg(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
