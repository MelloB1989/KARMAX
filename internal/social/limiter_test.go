package social

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRecorder is the store, without one.
type fakeRecorder struct {
	posts  []string
	last   time.Time
	count  int
	failOn string
}

func (f *fakeRecorder) RecordPost(platform, status, postID, text, detail string) error {
	f.posts = append(f.posts, platform+" "+status+" "+text)
	return nil
}

func (f *fakeRecorder) CountPostsSince(platform string, since time.Time) (int, error) {
	if f.failOn == "count" {
		return 0, errors.New("database is locked")
	}
	return f.count, nil
}

func (f *fakeRecorder) LastPostAt(platform string) (time.Time, error) {
	if f.failOn == "last" {
		return time.Time{}, errors.New("database is locked")
	}
	return f.last, nil
}

func TestItPostsWhenNothingIsInTheWay(t *testing.T) {
	rec := &fakeRecorder{}
	lim := &Limiter{Rec: rec, PerDay: 2, MinGap: time.Hour}

	out, err := Publish("x", Guard{MaxRunes: 280}, lim, "Spent the day on a sandbox and it finally holds.",
		func() (string, string, error) { return "123", "https://x.com/i/status/123", nil })
	if err != nil {
		t.Fatalf("an ordinary post was refused: %v", err)
	}
	if out["id"] != "123" {
		t.Errorf("id = %v, want 123", out["id"])
	}
	if len(rec.posts) != 1 || !strings.Contains(rec.posts[0], "posted") {
		t.Errorf("the post was not recorded: %v", rec.posts)
	}
}

func TestItStopsAtTheDailyLimit(t *testing.T) {
	rec := &fakeRecorder{count: 2}
	lim := &Limiter{Rec: rec, PerDay: 2, MinGap: time.Minute}

	called := false
	_, err := Publish("x", Guard{MaxRunes: 280}, lim, "One more thought.",
		func() (string, string, error) { called = true; return "1", "", nil })
	if err == nil {
		t.Fatal("the third post of the day went out")
	}
	if called {
		t.Error("it called the platform anyway")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestItSpacesPostsOut(t *testing.T) {
	rec := &fakeRecorder{count: 0, last: time.Now().Add(-20 * time.Minute)}
	lim := &Limiter{Rec: rec, PerDay: 5, MinGap: 3 * time.Hour}

	_, err := Publish("x", Guard{MaxRunes: 280}, lim, "Another one, twenty minutes later.",
		func() (string, string, error) { return "1", "", nil })
	if err == nil {
		t.Fatal("two posts twenty minutes apart were allowed with a three hour gap configured")
	}
	// The operator reads this. It should say how long to wait.
	if !strings.Contains(err.Error(), "early") {
		t.Errorf("the refusal does not say how early it was: %v", err)
	}
}

func TestTheKillSwitchStopsEverything(t *testing.T) {
	rec := &fakeRecorder{}
	lim := &Limiter{
		Rec: rec, PerDay: 10, MinGap: 0,
		Disabled: func() (bool, string) { return true, "switched off while I am on holiday" },
	}

	called := false
	_, err := Publish("linkedin", Guard{MaxRunes: 3000}, lim, "A perfectly fine post.",
		func() (string, string, error) { called = true; return "1", "", nil })
	if err == nil {
		t.Fatal("the kill switch did not stop the post")
	}
	if called {
		t.Error("it posted anyway")
	}
	if !strings.Contains(err.Error(), "holiday") {
		t.Errorf("the reason the operator gave was not shown back: %v", err)
	}
}

// A post that cannot be counted must not go out. The tempting failure here is
// to post anyway "because the limiter is only advisory" — which spends an
// unbounded budget exactly when something is already wrong.
func TestAStorageFailureBlocksThePost(t *testing.T) {
	for _, failOn := range []string{"count", "last"} {
		rec := &fakeRecorder{failOn: failOn}
		lim := &Limiter{Rec: rec, PerDay: 2, MinGap: time.Hour}

		called := false
		_, err := Publish("x", Guard{MaxRunes: 280}, lim, "Something worth saying.",
			func() (string, string, error) { called = true; return "1", "", nil })
		if err == nil {
			t.Fatalf("%s failing did not stop the post", failOn)
		}
		if called {
			t.Errorf("%s failing still posted", failOn)
		}
	}
}

// Refusals are recorded too, so the operator can see what KARMAX tried to say.
func TestARefusedDraftIsRecorded(t *testing.T) {
	rec := &fakeRecorder{}
	lim := &Limiter{Rec: rec, PerDay: 5, MinGap: 0}

	_, err := Publish("x", Guard{MaxRunes: 280, Forbidden: []string{"CampX"}}, lim,
		"Shipped the first VAPT report for CampX today.",
		func() (string, string, error) { return "1", "", nil })
	if err == nil {
		t.Fatal("a post naming a client was published")
	}
	if len(rec.posts) != 1 || !strings.Contains(rec.posts[0], "refused") {
		t.Fatalf("the refusal was not recorded: %v", rec.posts)
	}
	// And the text is kept, because "it refused something" without saying what
	// is not something anybody can act on.
	if !strings.Contains(rec.posts[0], "CampX") {
		t.Errorf("the refused draft was not kept: %v", rec.posts[0])
	}
}

// A platform failure is recorded as failed rather than posted, so a retry is
// not blocked by a post that never happened.
func TestAFailedPostDoesNotCountAsPosted(t *testing.T) {
	rec := &fakeRecorder{}
	lim := &Limiter{Rec: rec, PerDay: 2, MinGap: 0}

	_, err := Publish("x", Guard{MaxRunes: 280}, lim, "Fine text.",
		func() (string, string, error) { return "", "", errors.New("503 from the platform") })
	if err == nil {
		t.Fatal("a platform failure was reported as success")
	}
	if !strings.Contains(rec.posts[0], "failed") {
		t.Errorf("recorded as %q, want failed", rec.posts[0])
	}
}

// The order matters: privacy is checked before the rate limit, so a draft that
// names somebody is told so even on a day when nothing could post anyway.
func TestPrivacyIsCheckedBeforeTheRateLimit(t *testing.T) {
	rec := &fakeRecorder{count: 99}
	lim := &Limiter{Rec: rec, PerDay: 1, MinGap: time.Hour}

	_, err := Publish("x", Guard{MaxRunes: 280, Forbidden: []string{"Siva"}}, lim,
		"Long call with Siva about the rollout.",
		func() (string, string, error) { return "1", "", nil })
	if err == nil {
		t.Fatal("nothing was refused")
	}
	if !strings.Contains(err.Error(), "names somebody") {
		t.Errorf("the rate limit answered first, so the model never learns the draft was unpublishable: %v", err)
	}
}
