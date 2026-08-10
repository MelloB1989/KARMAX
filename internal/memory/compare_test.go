package memory

import (
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The floor local retrieval must clear.
//
// Set from what it measurably does today, not from an aspiration — a floor
// nobody can meet gets deleted, and a floor set at zero protects nothing. If a
// change pushes local below this, it has quietly turned the offline path into a
// degraded mode, and that is what this test exists to catch.
// Measured today: 60% top-1, 90% top-3, 90% top-5, MRR 0.75, p50 434µs.
// The floor sits just under that — enough headroom that a harmless change does
// not trip it, tight enough that losing a case does.
const (
	floorTop3 = 0.80
	floorTop5 = 0.85
	floorMRR  = 0.65
)

// corpus is realistic: the memories are the shape a real operator accumulates,
// including several that share vocabulary, because retrieval that only works on
// a corpus with no near-misses is not retrieval.
var corpus = []MemoryEntry{
	{ID: "vendor-terms", Content: "The vendor agreed to net-30 payment terms on 12 June after we pushed back on net-15."},
	{ID: "vendor-contact", Content: "Priya is the vendor's account manager; her colleague Rahul handles invoicing questions."},
	{ID: "campx-deal", Content: "CampX deal is 2 lakh for 6 VAPT reports over 3 months. Srikanth is the CTO, Siva runs day-to-day."},
	{ID: "campx-invoice", Content: "CampX invoice for the first installment was sent on 28 June and is not yet paid."},
	{ID: "laptop", Content: "The work laptop is a ThinkPad X1 with 32GB of RAM; the warranty runs out in March."},
	{ID: "deploy-process", Content: "Deploys go out through GitHub Actions on merge to main. Rollback is redeploying the previous tag."},
	{ID: "db-backup", Content: "The SQLite database is backed up nightly to S3 at 02:00 UTC by a cron on the host."},
	{ID: "office-wifi", Content: "The office wifi password is on the whiteboard in the meeting room, rotated every quarter."},
	{ID: "standup", Content: "Standup moved to 9:30am on Tuesdays because Siva is in a different timezone."},
	{ID: "gitloom-latency", Content: "GitLoom retrieval was 3.29 seconds with git provenance attached and 0.40 without it."},
	{ID: "mesh-design", Content: "An instance IS its Ed25519 key on the mesh. Names are untrusted labels shown to the operator."},
	{ID: "harness-choice", Content: "KARMAX uses codex as its brain because the Kiro gateway denies Claude models."},
}

// cases are questions asked the way a person asks them, not by quoting the
// memory back. That is the whole difficulty.
var cases = []Case{
	{Query: "what payment terms did we agree with the vendor", Want: []string{"vendor-terms"},
		Why: "direct, shares vocabulary"},
	{Query: "who do I talk to about a vendor invoice", Want: []string{"vendor-contact"},
		Why: "must prefer the contact over the terms, which also says vendor"},
	{Query: "how much is the CampX contract worth", Want: []string{"campx-deal"},
		Why: "'worth' does not appear in the memory"},
	{Query: "has CampX paid us yet", Want: []string{"campx-invoice"},
		Why: "two CampX memories; must pick the one about payment"},
	{Query: "how do I roll back a bad release", Want: []string{"deploy-process"},
		Why: "'release' vs 'deploy'"},
	{Query: "where are the database backups stored", Want: []string{"db-backup"},
		Why: "direct"},
	{Query: "when is standup", Want: []string{"standup"},
		Why: "short query"},
	{Query: "why is the daily meeting not at 9", Want: []string{"standup"},
		Why: "'daily meeting' never appears; this is the one only embeddings should get"},
	{Query: "what identifies an instance on the mesh", Want: []string{"mesh-design"},
		Why: "direct"},
	{Query: "which model does KARMAX run on", Want: []string{"harness-choice"},
		Why: "'run on' vs 'brain'"},
}

func corpusManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "k.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	m := NewManager("test", "test-ns", dir, db, zap.NewNop())
	for _, e := range corpus {
		if err := m.Write(e); err != nil {
			t.Fatalf("write %s: %v", e.ID, err)
		}
	}
	return m
}

// The hard line from open question 2, enforced rather than intended.
func TestLocalRetrievalStaysFirstClass(t *testing.T) {
	m := corpusManager(t)
	score := Compare("local", m.SearchSemantic, cases)
	t.Log("\n" + Report(score))

	if score.Recall3() < floorTop3 {
		t.Errorf("local top-3 recall fell to %.0f%%, floor is %.0f%%",
			score.Recall3()*100, floorTop3*100)
	}
	if score.Recall5() < floorTop5 {
		t.Errorf("local top-5 recall fell to %.0f%%, floor is %.0f%%",
			score.Recall5()*100, floorTop5*100)
	}
	if score.MRR < floorMRR {
		t.Errorf("local MRR fell to %.2f, floor is %.2f", score.MRR, floorMRR)
	}
}

// Offline is the property the open-source path is sold on, so it gets its own
// assertion rather than being implied by the one above.
func TestRetrievalWorksWithNoRemoteConfigured(t *testing.T) {
	m := corpusManager(t)
	if m.remote != nil {
		t.Fatal("a plain manager should have no remote")
	}
	results, err := m.SearchSemantic("what payment terms did we agree with the vendor", 5)
	if err != nil {
		t.Fatalf("offline retrieval failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("offline retrieval returned nothing")
	}
}

// A near-miss corpus is where keyword retrieval usually fails: two memories
// share a subject and only one answers the question.
func TestRetrievalDistinguishesNearMisses(t *testing.T) {
	m := corpusManager(t)
	for _, c := range []Case{
		{Query: "has CampX paid us yet", Want: []string{"campx-invoice"}},
		{Query: "who handles vendor invoicing", Want: []string{"vendor-contact"}},
	} {
		results, err := m.SearchSemantic(c.Query, 5)
		if err != nil {
			t.Fatal(err)
		}
		if rankOfAny(results, c.Want) == 0 {
			t.Errorf("%q did not surface %v at all", c.Query, c.Want)
		}
	}
}
