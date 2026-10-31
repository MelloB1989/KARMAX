package awsdash

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// keys is an IAM access key. Its values go to the CLI's environment and
// nowhere else: no log, error or report holds them.
type keys struct{ id, secret string }

// readKeys reads the CSV the IAM console downloads for an access key: a
// header naming "Access key ID" and "Secret access key", then their values.
// Errors name the file and what is missing, never a value.
func readKeys(path string) (keys, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return keys{}, fmt.Errorf("keys_csv: %w", err)
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return keys{}, fmt.Errorf("keys_csv %s: not a CSV", path)
	}
	if len(rows) < 2 {
		return keys{}, fmt.Errorf("keys_csv %s: want a header and a row of keys", path)
	}
	idCol, secretCol := -1, -1
	for i, h := range rows[0] {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "access key id":
			idCol = i
		case "secret access key":
			secretCol = i
		}
	}
	if idCol < 0 || secretCol < 0 {
		return keys{}, fmt.Errorf("keys_csv %s: the header has no Access key ID and Secret access key columns", path)
	}
	row := rows[1]
	if max(idCol, secretCol) >= len(row) {
		return keys{}, fmt.Errorf("keys_csv %s: the second row is short", path)
	}
	k := keys{id: strings.TrimSpace(row[idCol]), secret: strings.TrimSpace(row[secretCol])}
	if k.id == "" || k.secret == "" {
		return keys{}, fmt.Errorf("keys_csv %s: the key ID or secret is empty", path)
	}
	return k, nil
}

// FindCLI is the aws CLI to run: the configured one, else the PATH's, else
// ~/.local/bin/aws, where its installer puts it (systemd's PATH lacks it).
func FindCLI(configured, home string) string {
	if configured != "" {
		return configured
	}
	if p, err := exec.LookPath("aws"); err == nil {
		return p
	}
	return filepath.Join(home, ".local", "bin", "aws")
}

// Runner runs the aws CLI with args and returns what it printed (JSON).
// Tests fake it.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// cli runs the real aws CLI with the keys in its environment.
type cli struct {
	path string
	env  []string
	k    keys
}

func newCLI(path string, k keys) cli {
	// None of the caller's AWS settings: a profile, a region or a config
	// file would change what the keys read, or whose they are.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AWS_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"AWS_ACCESS_KEY_ID="+k.id,
		"AWS_SECRET_ACCESS_KEY="+k.secret,
		"AWS_DEFAULT_OUTPUT=json",
		"AWS_PAGER=",
		"AWS_CONFIG_FILE="+os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE="+os.DevNull,
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CLI_AUTO_PROMPT=off",
		"AWS_RETRY_MODE=standard",
		"AWS_MAX_ATTEMPTS=3",
	)
	return cli{path: path, env: env, k: k}
}

func (c cli) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.path, args...)
	cmd.Env = c.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no answer in time")
		}
		msg := lastLine(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(c.blank(cliError(msg)))
	}
	return out, nil
}

// anyKeyID is an access key ID, anyone's.
var anyKeyID = regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)

// blank takes every key out of s.
func (c cli) blank(s string) string {
	for _, v := range []string{c.k.secret, c.k.id} {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[key]")
		}
	}
	return anyKeyID.ReplaceAllString(s, "[key]")
}

// cliError is the CLI's "An error occurred (Code) when calling the Op
// operation: message" as "Code: message".
var cliErr = regexp.MustCompile(`^An error occurred \(([^)]+)\)(?: when calling the \S+ operation)?(?: \([^)]*\))?: ?`)

func cliError(s string) string {
	if m := cliErr.FindStringSubmatch(s); m != nil {
		return m[1] + ": " + s[len(m[0]):]
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
