package awsdash

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Made-up keys, shaped like real ones.
const (
	testID     = "AKIAEXAMPLEKEY000001"
	testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The console's download: a BOM, CRLF line ends, the header, then the keys.
func TestReadKeysTakesTheConsolesCSV(t *testing.T) {
	for name, body := range map[string]string{
		"console": "\xef\xbb\xbfAccess key ID,Secret access key\r\n" + testID + "," + testSecret + "\r\n",
		"plain":   "Access key ID,Secret access key\n" + testID + "," + testSecret + "\n",
		"swapped": "Secret access key,Access key ID\n" + testSecret + "," + testID + "\n",
		"user":    "User name,Access key ID,Secret access key\nfleet-reader," + testID + "," + testSecret + "\n",
	} {
		k, err := readKeys(writeFile(t, "keys.csv", body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if k.id != testID || k.secret != testSecret {
			t.Errorf("%s: read the wrong keys", name)
		}
	}
}

// A bad file says what is wrong with it, never what is in it.
func TestReadKeysNeverSaysTheKeys(t *testing.T) {
	for name, body := range map[string]string{
		"no row":    "Access key ID,Secret access key\n",
		"no secret": "Access key ID,Secret access key\n" + testID + ",\n",
		"no header": testID + "," + testSecret + "\n",
	} {
		_, err := readKeys(writeFile(t, "keys.csv", body))
		if err == nil {
			t.Errorf("%s: read", name)
			continue
		}
		if strings.Contains(err.Error(), testID) || strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: the error shows a key: %v", name, err)
		}
	}
	if _, err := readKeys(filepath.Join(t.TempDir(), "missing.csv")); err == nil {
		t.Error("a missing file read")
	}
}

// The CLI gets the keys in its environment only, and none of the caller's
// AWS settings (a profile would win over them).
func TestTheCLIGetsTheKeysThroughItsEnvironment(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	aws := writeFile(t, "aws", "#!/bin/sh\n"+
		`printf '{"args":"%s","id":"%s","secret":"%s","out":"%s","pager":"[%s]","profile":"%s"}' "$*" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" "$AWS_DEFAULT_OUTPUT" "$AWS_PAGER" "$AWS_PROFILE"`+"\n")
	if err := os.Chmod(aws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_PROFILE", "someone-else")
	run := newCLI(aws, keys{id: testID, secret: testSecret})
	out, err := run.Run(context.Background(), "sts", "get-caller-identity")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"args":"sts get-caller-identity","id":"` + testID + `","secret":"` + testSecret + `","out":"json","pager":"[]","profile":""}`
	if string(out) != want {
		t.Errorf("the CLI saw %s", out)
	}
}

// What the CLI says on a failure is cut to its point, with any key in it
// blanked out.
func TestACLIFailureIsShortAndKeyless(t *testing.T) {
	aws := writeFile(t, "aws", "#!/bin/sh\n"+
		"echo >&2\n"+
		"echo 'An error occurred (SignatureDoesNotMatch) when calling the ListFunctions operation: Credential="+testID+"/20261025 secret "+testSecret+" and AKIAOTHEREXAMPLE0002 do not match' >&2\n"+
		"exit 254\n")
	if err := os.Chmod(aws, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := newCLI(aws, keys{id: testID, secret: testSecret}).Run(context.Background(), "lambda", "list-functions")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	if strings.Contains(msg, testID) || strings.Contains(msg, testSecret) || strings.Contains(msg, "AKIAOTHEREXAMPLE0002") {
		t.Errorf("the error shows a key: %s", msg)
	}
	if !strings.HasPrefix(msg, "SignatureDoesNotMatch: Credential=") {
		t.Errorf("error = %q", msg)
	}
}

func TestFindCLI(t *testing.T) {
	home := t.TempDir()
	if got := FindCLI("/opt/aws/bin/aws", home); got != "/opt/aws/bin/aws" {
		t.Errorf("configured: %s", got)
	}
	bin := t.TempDir()
	onPath := filepath.Join(bin, "aws")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got := FindCLI("", home); got != onPath {
		t.Errorf("on PATH: %s", got)
	}
	// systemd's PATH has no ~/.local/bin, where the installer puts it.
	t.Setenv("PATH", t.TempDir())
	if got := FindCLI("", home); got != filepath.Join(home, ".local", "bin", "aws") {
		t.Errorf("fallback: %s", got)
	}
}
