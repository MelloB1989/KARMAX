package youtrack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func creds(kv map[string]string) connectorkit.Credentials {
	return connectorkit.Credentials{Config: kv}
}

// Half of setup failures are a base_url that already ends in /api or carries a
// trailing slash. Both are unambiguous, so they are accepted rather than
// bounced back as a config error the operator has to think about.
func TestBaseURLToleratesWhatPeoplePaste(t *testing.T) {
	want := "https://acme.youtrack.cloud/api"
	for _, given := range []string{
		"https://acme.youtrack.cloud",
		"https://acme.youtrack.cloud/",
		"https://acme.youtrack.cloud/api",
		"https://acme.youtrack.cloud/api/",
		"  https://acme.youtrack.cloud  ",
		"acme.youtrack.cloud",
	} {
		got, err := base(creds(map[string]string{"base_url": given}))
		if err != nil {
			t.Errorf("%q: %v", given, err)
			continue
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", given, got, want)
		}
	}

	if _, err := base(creds(map[string]string{})); err == nil {
		t.Error("an empty base_url was accepted")
	}
}

// YouTrack sends epoch MILLISECONDS. Reading them as seconds lands in the year
// 58000, which looks like a downstream parsing bug rather than a units one.
func TestTimestampsAreMilliseconds(t *testing.T) {
	got := millisToRFC3339(1787810740000)
	if !strings.HasPrefix(got, "2026-") {
		t.Errorf("got %q — milliseconds were probably read as seconds", got)
	}
	if millisToRFC3339(0) != "" {
		t.Error("a zero timestamp should render as empty, not as 1970")
	}
}

// A custom field is a tagged union. Handing the raw JSON to a model wastes
// context on wrappers and invites it to quote an internal id back at a human.
func TestCustomFieldsCollapseToSomethingReadable(t *testing.T) {
	cases := map[string]any{
		`{"name":"In Progress"}`:                 "In Progress",
		`{"login":"alice","fullName":"Alice A"}`: "alice",
		`{"fullName":"Bob B"}`:                   "Bob B",
		`{"text":"some notes"}`:                  "some notes",
		`{"minutes":90}`:                         90,
		`null`:                                   nil,
		`""`:                                     nil,
	}
	for raw, want := range cases {
		got := simpleValue(json.RawMessage(raw))
		if raw == `""` {
			// An empty string is not a useful value; nil keeps it out of output.
			if got != nil && got != "" {
				t.Errorf("%s -> %#v", raw, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s -> %#v, want %#v", raw, got, want)
		}
	}

	multi := simpleValue(json.RawMessage(`[{"name":"bug"},{"name":"regression"}]`))
	names, ok := multi.([]string)
	if !ok || len(names) != 2 || names[0] != "bug" {
		t.Errorf("a multi-value field collapsed to %#v", multi)
	}
}

func TestAnIssueRendersWithoutYouTrackInternals(t *testing.T) {
	var i issue
	raw := `{
	  "idReadable":"LAMB-42","summary":"Fix the thing","description":"body",
	  "created":1787810740000,"updated":1787810750000,
	  "project":{"shortName":"LAMB"},"reporter":{"login":"kartik"},
	  "customFields":[
	    {"name":"State","value":{"name":"Open"}},
	    {"name":"Assignee","value":{"login":"alice"}},
	    {"name":"Story points","value":null}
	  ]
	}`
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	f := i.flatten()

	if f["id"] != "LAMB-42" || f["project"] != "LAMB" || f["reporter"] != "kartik" {
		t.Errorf("core fields wrong: %#v", f)
	}
	if f["state"] != "Open" {
		t.Errorf("State did not flatten to state=Open: %#v", f["state"])
	}
	if f["assignee"] != "alice" {
		t.Errorf("Assignee did not flatten: %#v", f["assignee"])
	}
	// A null custom field must be absent, not present-and-null: the model reads
	// a null as "this is set to nothing" rather than "not set".
	if _, present := f["story_points"]; present {
		t.Error("a null custom field was included")
	}
	if f["resolved"] != false {
		t.Error("an unresolved issue should report resolved=false")
	}
}

// A 401 that just says "request failed" sends the operator to the logs. The
// status is translated where the meaning is unambiguous.
func TestErrorsSayWhatToDo(t *testing.T) {
	for status, want := range map[int]string{
		401: "expired or revoked",
		403: "lacks permission",
		404: "check the issue id",
	} {
		err := apiError(status, []byte(`{"error_description":"nope"}`))
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%d -> %q, expected it to mention %q", status, err, want)
		}
	}
}

func TestManifestDeclaresWhatSetupNeeds(t *testing.T) {
	m := New("").Manifest()
	if m.ID != "youtrack" {
		t.Errorf("id is %q", m.ID)
	}
	required := map[string]bool{}
	for _, f := range m.Config {
		if f.Required {
			required[f.Key] = true
		}
		if f.Key == "token" && !f.Secret {
			t.Error("the token field is not marked secret — it would be echoed back and logged")
		}
	}
	for _, k := range []string{"base_url", "token"} {
		if !required[k] {
			t.Errorf("%s should be required", k)
		}
	}
}

// An org with two YouTrack instances has two of everything, and the agent has
// to act as the right one rather than whichever token loaded first.
func TestASecondInstanceIsQualifiedByAccount(t *testing.T) {
	primary, work := New(""), New("work")

	if got := primary.Manifest().ID; got != "youtrack" {
		t.Errorf("the primary instance should be plain 'youtrack', got %q", got)
	}
	if got := work.Manifest().ID; got != "youtrack:work" {
		t.Errorf("a named instance should be 'youtrack:work', got %q", got)
	}

	names := map[string]bool{}
	for _, tool := range work.Tools() {
		names[tool.Name] = true
	}
	if !names["youtrack.search@work"] {
		t.Errorf("a named instance's tools are not qualified: %v", names)
	}
	for _, tool := range primary.Tools() {
		if strings.Contains(tool.Name, "@") {
			t.Errorf("the primary instance qualified a tool name: %q", tool.Name)
		}
	}
}
