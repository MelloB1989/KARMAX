package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// A fake API key: it must never turn up anywhere but a read's answer.
const apiKey = "sk-test-0123456789abcdef"

func putSecret(t *testing.T, f *Fleet, name, value string) {
	t.Helper()
	if err := f.SecretPut(name, []byte(value), t0); err != nil {
		t.Fatal(err)
	}
}

func assignAs(t *testing.T, f *Fleet, agent, task, from string) {
	t.Helper()
	if err := f.Assign(context.Background(), agent, task, from, "", t0); err != nil {
		t.Fatal(err)
	}
}

// leaks fails the test when the value is in the ledger's files (the
// database and its write-ahead log), its events, or what ls says.
func leaks(t *testing.T, f *Fleet, value string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.cfg.StateDir, "fleet.db*"))
	if len(files) == 0 {
		t.Fatal("no ledger files to look in")
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(value)) {
			t.Errorf("%s holds the value in the clear", filepath.Base(p))
		}
	}
	evs, err := f.db.Events("", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(e.Detail, value) {
			t.Errorf("event %s holds the value: %s", e.Kind, e.Detail)
		}
	}
	ss, err := f.Secrets(t0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := jsonOf(ss); strings.Contains(b, value) {
		t.Errorf("ls holds the value: %s", b)
	}
}

func TestASecretGrantedToATaskIsReadByItsAgentAndNowhereElse(t *testing.T) {
	f, _, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	assignAs(t, f, "agent-03", "T1", "operator")
	if err := f.SecretGrant("openai", "T1", "", "operator", 0, t0); err != nil {
		t.Fatal(err)
	}
	v, err := f.SecretRead("agent-03", "openai", t0)
	if err != nil || string(v) != apiKey {
		t.Fatalf("read = %q, %v", v, err)
	}
	ss, _ := f.Secrets(t0)
	if len(ss) != 1 || ss[0].Name != "openai" || ss[0].ReadAgent != "agent-03" || ss[0].ReadTask != "T1" ||
		len(ss[0].Grants) != 1 || ss[0].Grants[0].Task != "T1" {
		t.Fatalf("ls = %+v", ss)
	}
	evs, _ := f.db.EventsWhere(ledger.EventQuery{Kinds: []string{"fleet.secret.read"}})
	if len(evs) != 1 || evs[0].Agent != "agent-03" || !strings.Contains(evs[0].Detail, `"secret":"openai"`) ||
		!strings.Contains(evs[0].Detail, `"task":"T1"`) {
		t.Fatalf("read events = %+v", evs)
	}
	leaks(t, f, apiKey)
	if f.db.Close() == nil { // and on disk, once the log is folded in
		db, err := ledger.Open(filepath.Join(f.cfg.StateDir, "fleet.db"))
		if err != nil {
			t.Fatal(err)
		}
		f.db = db
		t.Cleanup(func() { db.Close() })
	}
	leaks(t, f, apiKey)
}

func TestTheKeyFileIsMadeOnFirstUseAndOnlyYoursToRead(t *testing.T) {
	f, _, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	st, err := os.Stat(filepath.Join(f.cfg.StateDir, "secrets.key"))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("key file: %v %v", st, err)
	}
}

func TestATaskGrantEndsWhenTheTaskClosesHoweverItCloses(t *testing.T) {
	for _, how := range []string{"done", "close", "superseded", "cancel"} {
		t.Run(how, func(t *testing.T) {
			f, _, _ := newFleet(t)
			putSecret(t, f, "openai", apiKey)
			assignAs(t, f, "agent-03", "T1", "operator")
			if err := f.SecretGrant("openai", "T1", "", "operator", 0, t0); err != nil {
				t.Fatal(err)
			}
			var err error
			switch how {
			case "done":
				err = f.Done("agent-03", "T1", "", "", "", t0)
			case "close":
				err = f.Close("agent-03", "T1", "", "", "", t0)
			case "superseded":
				err = f.Assign(context.Background(), "agent-03", "T2", "operator", "", t0)
			case "cancel":
				err = f.Cancel(context.Background(), "T1", "operator", "", t0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.SecretRead("agent-03", "openai", t0); !errors.Is(err, ErrForbidden) {
				t.Fatalf("read after %s: %v", how, err)
			}
			// Reopened under the same id, the task does not get it back.
			assignAs(t, f, "agent-03", "T1", "operator")
			if _, err := f.SecretRead("agent-03", "openai", t0); !errors.Is(err, ErrForbidden) {
				t.Fatalf("read after the task was given again: %v", err)
			}
			if ss, _ := f.Secrets(t0); len(ss[0].Grants) != 0 {
				t.Fatalf("grants after %s: %+v", how, ss[0].Grants)
			}
		})
	}
}

func TestAGrantWithATTLEndsWhenItPasses(t *testing.T) {
	f, _, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	assignAs(t, f, "agent-03", "T1", "operator")
	if err := f.SecretGrant("openai", "", "agent-03", "operator", 2*time.Hour, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-03", "openai", t0.Add(119*time.Minute)); err != nil {
		t.Fatalf("read within the ttl: %v", err)
	}
	if _, err := f.SecretRead("agent-03", "openai", t0.Add(2*time.Hour)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read at the ttl: %v", err)
	}
	if ss, _ := f.Secrets(t0.Add(2 * time.Hour)); len(ss[0].Grants) != 0 {
		t.Fatalf("grants after the ttl: %+v", ss[0].Grants)
	}
}

// Who may use a secret is the ledger's word: the agent's own open task and
// who assigned it, never anything the agent says.
func TestOnlyAnAgentOnTheOperatorsWorkReadsAGrantedSecret(t *testing.T) {
	cases := []struct {
		assigner string
		may      bool
	}{
		{"operator", true},
		{"karmax", true},              // the orchestrator
		{"relay-from-operator", true}, // an operator's session: only the full scope names an assigner
		{"op@example.com", true},      // a tailnet operator, by login
		{"ram@example.com", false},    // a member
		{"stranger@example.com", false},
	}
	for _, c := range cases {
		t.Run(c.assigner, func(t *testing.T) {
			f, _, _ := newFleet(t)
			f.cfg.Orchestrator.Name = "karmax"
			f.cfg.Tailnet = &config.Tailnet{Operators: []string{"op@example.com"}, Members: []string{"ram@example.com"}}
			putSecret(t, f, "openai", apiKey)
			assignAs(t, f, "agent-03", "T1", c.assigner)
			if err := f.SecretGrant("openai", "", "agent-03", "operator", 0, t0); err != nil {
				t.Fatal(err)
			}
			v, err := f.SecretRead("agent-03", "openai", t0)
			if c.may && (err != nil || string(v) != apiKey) {
				t.Fatalf("read = %q, %v", v, err)
			}
			if !c.may && (!errors.Is(err, ErrForbidden) || v != nil) {
				t.Fatalf("a task %s assigned was let read: %q, %v", c.assigner, v, err)
			}
		})
	}
}

func TestAGrantToOneAgentIsNotAnothersAndNoTaskIsNoRead(t *testing.T) {
	f, _, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	if err := f.SecretGrant("openai", "", "agent-05", "operator", 0, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-05", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent with no task read it: %v", err)
	}
	assignAs(t, f, "agent-03", "T1", "operator")
	if _, err := f.SecretRead("agent-03", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent-03 read agent-05's grant: %v", err)
	}
	if _, err := f.SecretRead("agent-99", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an unknown agent: %v", err)
	}
	evs, _ := f.db.EventsWhere(ledger.EventQuery{Kinds: []string{"fleet.secret.denied"}})
	if len(evs) != 3 {
		t.Fatalf("denied events = %+v", evs)
	}
	leaks(t, f, apiKey)
}

func TestRevokeAndRemoveEndReads(t *testing.T) {
	f, _, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	assignAs(t, f, "agent-03", "T1", "operator")
	_ = f.SecretGrant("openai", "", "agent-03", "operator", 0, t0)
	if err := f.SecretRevoke("openai", "", "agent-03"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-03", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read after revoke: %v", err)
	}
	if err := f.SecretRevoke("openai", "", "agent-03"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("revoke twice: %v", err)
	}
	_ = f.SecretGrant("openai", "T1", "", "operator", 0, t0)
	if err := f.SecretRemove("openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SecretRead("agent-03", "openai", t0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read after rm: %v", err)
	}
	if ss, _ := f.Secrets(t0); len(ss) != 0 {
		t.Fatalf("ls after rm = %+v", ss)
	}
	if err := f.SecretGrant("openai", "T1", "", "operator", 0, t0); !errors.Is(err, ledger.ErrNoSecret) {
		t.Fatalf("grant of a removed secret: %v", err)
	}
}

func TestNoErrorQuotesAValue(t *testing.T) {
	f, _, _ := newFleet(t)
	errs := []error{
		f.SecretPut("bad name", []byte(apiKey), t0),
		f.SecretPut("big", bytes.Repeat([]byte(apiKey), 5000), t0),
	}
	putSecret(t, f, "openai", apiKey)
	_, err := f.SecretRead("agent-03", "openai", t0)
	errs = append(errs, err, f.SecretGrant("openai", "T1", "agent-03", "operator", 0, t0))
	for _, err := range errs {
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), "sk-test") {
			t.Errorf("error quotes the value: %v", err)
		}
	}
}

func TestAGrantedValueInAMessageIsRedactedBeforeItIsStoredOrDelivered(t *testing.T) {
	f, boxes, _ := newFleet(t)
	putSecret(t, f, "openai", apiKey)
	putSecret(t, f, "deploy-key", "-----BEGIN KEY-----\nAAAAB3NzaC1yc2EAAAADAQABAAABgQC0\nBBBBB3NzaC1yc2EAAAADAQABAAABgQC1\n-----END KEY-----\n")
	assignAs(t, f, "agent-03", "T1", "operator")
	_ = f.SecretGrant("openai", "T1", "", "operator", 0, t0)
	ctx := context.Background()
	if err := f.Tell(ctx, "agent-03", "operator", "done; the key was "+apiKey+" fyi", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.Tell(ctx, "agent-03", "agent-05", "use "+apiKey+" and BBBBB3NzaC1yc2EAAAADAQABAAABgQC1", t0); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.Inbox(0)
	if len(msgs) != 1 || msgs[0].Text != "done; the key was [secret:openai] fyi" {
		t.Fatalf("inbox = %+v", msgs)
	}
	want := "relay agent-03→agent-05: use [secret:openai] and [secret:deploy-key]"
	if !slices.Contains(boxes["agent-05"].calls, want) {
		t.Fatalf("relayed = %v, want %q", boxes["agent-05"].calls, want)
	}
	thread, _ := f.db.TaskThread("T1")
	for _, r := range thread {
		if strings.Contains(r.Text, apiKey) {
			t.Fatalf("the ledger kept it: %q", r.Text)
		}
	}
	leaks(t, f, apiKey)
}

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
