package runtime

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

func splitAll(deltas ...string) []string {
	var sp sentenceSplitter
	var out []string
	for _, d := range deltas {
		out = append(out, sp.Push(d)...)
	}
	return append(out, sp.Flush()...)
}

func TestSplitterSentences(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"two sentences", []string{"Sure, I can do that. It will take a minute."}, []string{"Sure, I can do that.", "It will take a minute."}},
		{"across deltas", []string{"Sure, I can ", "do that", ". It will", " take a minute."}, []string{"Sure, I can do that.", "It will take a minute."}},
		{"decimal not split", []string{"The limit is 2.5 million dollars. Fine."}, []string{"The limit is 2.5 million dollars.", "Fine."}},
		{"decimal split across deltas", []string{"It grew by 2.", "5 percent this quarter. Good."}, []string{"It grew by 2.5 percent this quarter.", "Good."}},
		{"abbreviation", []string{"Dr. Sharma called you about the meeting. Call back."}, []string{"Dr. Sharma called you about the meeting.", "Call back."}},
		{"dotted abbreviation", []string{"Use a tool, e.g. the planner, for that. Done."}, []string{"Use a tool, e.g. the planner, for that.", "Done."}},
		{"initial", []string{"I spoke with A. Kumar yesterday. He agreed."}, []string{"I spoke with A. Kumar yesterday.", "He agreed."}},
		{"tiny fragments merge", []string{"Okay. Sure. I will handle that for you now."}, []string{"Okay. Sure. I will handle that for you now."}},
		{"question and exclamation", []string{"Did you mean Friday? Great! I will book it."}, []string{"Did you mean Friday?", "Great! I will book it."}},
		{"devanagari danda", []string{"ठीक है, मैं यह कर दूँगा। आपका मीटिंग कल है।"}, []string{"ठीक है, मैं यह कर दूँगा।", "आपका मीटिंग कल है।"}},
		{"hinglish", []string{"Haan bilkul, main abhi check karta hoon. Kal ka meeting 3 baje hai."}, []string{"Haan bilkul, main abhi check karta hoon.", "Kal ka meeting 3 baje hai."}},
		{"remainder flushed", []string{"First sentence is complete. And a trailing bit"}, []string{"First sentence is complete.", "And a trailing bit"}},
		{"long clause breaks at comma", []string{strings.Repeat("word ", 17) + "end, and then the rest of it goes on"}, []string{strings.Repeat("word ", 17) + "end,", "and then the rest of it goes on"}},
	}
	for _, c := range cases {
		if got := splitAll(c.in...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestSplitterWaitsForTheNextCharacterAfterADot(t *testing.T) {
	var sp sentenceSplitter
	if got := sp.Push("The total is exactly 2."); len(got) != 0 {
		t.Fatalf("emitted before knowing the dot ends a sentence: %q", got)
	}
	if got := sp.Push("5 dollars today."); len(got) != 0 {
		t.Fatalf("decimal was split: %q", got)
	}
}

func TestSpeakableNormalisesForTheSynthesiser(t *testing.T) {
	cases := map[string]string{
		"The cap is $20 per month.":             "The cap is 20 dollars per month.",
		"It costs ₹2.5 lakh":                    "It costs 2.5 lakh rupees",
		"Qwen3-Next 80B is fast — very fast":    "Qwen3-Next 80 billion is fast, very fast",
		"Up 12% & rising":                       "Up 12 percent and rising",
		"Done 🎉 see you":                        "Done see you",
		"Pick one – or two":                     "Pick one, or two",
		"Read [the docs](https://x.io/a) first": "Read the docs first",
		"# Heading\n- one\n- two":               "Heading. one. two",
		"Wait 2–3 days, e.g. Monday":            "Wait 2 to 3 days, for example, Monday",
		"It is #1 for 5GB uploads":              "It is number 1 for 5 gigabytes uploads",
		"about ~20 people":                      "about about 20 people",
		"input/output ready":                    "input output ready",
		"“Quoted” text":                         `"Quoted" text`,
		"Allow it — , then go":                  "Allow it, then go",
	}
	for in, want := range cases {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	for _, in := range []string{"**bold** `code` _x_ ~~gone~~", "→ arrow ⭐ star", "a — b – c"} {
		got := speakable(in)
		for _, bad := range []string{"*", "`", "~", "—", "–", "→", "⭐"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q survived in %q", bad, got)
			}
		}
	}
}

// streamSession streams scripted deltas through the turn sink, then returns.
type streamSession struct {
	fakeSession
	sink   func(string)
	deltas []string
	// between runs after delta i is streamed, before the next one.
	between func(i int)
	hangup  func()
}

func (s *streamSession) SetTurnStream(fn func(string)) { s.sink = fn }
func (s *streamSession) Chat(_ context.Context, m string) (string, []karmahelper.ToolCallRecord, karmahelper.TokenInfo, error) {
	s.asked = append(s.asked, m)
	var all strings.Builder
	for i, d := range s.deltas {
		all.WriteString(d)
		if s.sink != nil {
			s.sink(d)
		}
		if s.between != nil {
			s.between(i)
		}
	}
	if s.hangup != nil {
		s.hangup()
	}
	return all.String(), nil, karmahelper.TokenInfo{}, nil
}
func (s *streamSession) SetHistory(h models.AIChatHistory) { s.hist = h }

type sayLog struct {
	mu    sync.Mutex
	said  []string
	allow func(n int) bool
}

func (l *sayLog) say(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.allow != nil && !l.allow(len(l.said)) {
		return false
	}
	l.said = append(l.said, text)
	return true
}

func (l *sayLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.said...)
}

func TestStreamingSpeaksFirstSentenceBeforeTheTurnEnds(t *testing.T) {
	log := &sayLog{}
	sess := &streamSession{deltas: []string{"Sure, I can do ", "that for you. ", "It will take ", "about a minute."}}
	var atFirstGap []string
	sess.between = func(i int) {
		if i == 2 {
			atFirstGap = log.all()
		}
	}
	b := newTestBrain(sess, nil)
	r, err := b.Answer(voice.WithSay(context.Background(), log.say), voice.Utterance{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(atFirstGap) != 1 || atFirstGap[0] != "Sure, I can do that for you." {
		t.Fatalf("first sentence was not spoken mid-turn: %q", atFirstGap)
	}
	if got := log.all(); !reflect.DeepEqual(got, []string{"Sure, I can do that for you.", "It will take about a minute."}) {
		t.Fatalf("spoken = %q", got)
	}
	if r.Text != "" {
		t.Fatalf("final reply re-sends streamed text: %q", r.Text)
	}
}

func TestUnstreamedReplyIsStillSentWhole(t *testing.T) {
	sess := &fakeSession{reply: "All **done**."}
	b := newTestBrain(sess, nil)
	r, _ := b.Answer(context.Background(), voice.Utterance{Text: "x"})
	if r.Text != "All done." {
		t.Fatalf("reply = %q", r.Text)
	}
}

func TestHangupStillFollowsAStreamedReply(t *testing.T) {
	log := &sayLog{}
	var b *voiceBrain
	sess := &streamSession{deltas: []string{"Goodbye, talk soon.", " Take care."}}
	sess.hangup = func() { b.hangup.Store(true) }
	b = newTestBrain(sess, nil)
	r, _ := b.Answer(voice.WithSay(context.Background(), log.say), voice.Utterance{Text: "bye"})
	if !r.Hangup || r.Text != "" || len(log.all()) != 2 {
		t.Fatalf("reply %+v, said %q", r, log.all())
	}
}

func TestInterruptedTurnStopsEmitting(t *testing.T) {
	log := &sayLog{allow: func(n int) bool { return n < 1 }}
	sess := &streamSession{deltas: []string{"One long sentence here. ", "Another long sentence here. ", "A third long sentence here."}}
	b := newTestBrain(sess, nil)
	r, _ := b.Answer(voice.WithSay(context.Background(), log.say), voice.Utterance{Text: "x"})
	if got := log.all(); len(got) != 1 {
		t.Fatalf("kept speaking after the turn went stale: %q", got)
	}
	if r.Text != "" {
		t.Fatalf("stale turn re-sent text: %q", r.Text)
	}
}

func TestStreamedTextHasNoMarkdownOrDashes(t *testing.T) {
	log := &sayLog{}
	sess := &streamSession{deltas: []string{"**Okay** — the cap is $20. ", "Ready & waiting."}}
	b := newTestBrain(sess, nil)
	_, _ = b.Answer(voice.WithSay(context.Background(), log.say), voice.Utterance{Text: "x"})
	if got := strings.Join(log.all(), " "); got != "Okay, the cap is 20 dollars. Ready and waiting." {
		t.Fatalf("spoken = %q", got)
	}
}

func TestOpeningFromBriefStreamsAndHangsUpOneWay(t *testing.T) {
	log := &sayLog{}
	var b *voiceBrain
	sess := &streamSession{deltas: []string{"Hi, calling about Friday. ", "Can you confirm?"}}
	sess.hangup = func() { b.hangup.Store(true) }
	b = newTestBrain(sess, nil)
	b.callBrief = "confirm Friday"
	if got := b.Greeting(voice.WithSay(context.Background(), log.say), "p"); got != "" {
		t.Fatalf("opening was returned as well as streamed: %q", got)
	}
	if len(log.all()) != 2 {
		t.Fatalf("said %q", log.all())
	}
	select {
	case n := <-b.notices:
		if !n.Hangup {
			t.Fatalf("notice %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no hangup notice")
	}
}

func TestLateLookupHitsJoinTheNextTurn(t *testing.T) {
	t.Setenv("KARMAX_VOICE_LOOKUP_MS", "10")
	if preAnswerBudget() != 10*time.Millisecond {
		t.Fatal("budget not configurable")
	}
	t.Setenv("KARMAX_VOICE_LOOKUP_MS", "")
	if preAnswerBudget() != 500*time.Millisecond {
		t.Fatal("default budget changed")
	}
	b := newTestBrain(&fakeSession{}, nil)
	b.late <- []string{"late fact"}
	if got := b.collectHits("anything"); len(got) != 1 || got[0] != "late fact" {
		t.Fatalf("hits = %q", got)
	}
}

func TestAudioTagsArePreservedOnlyWhenTheVoicePerformsThem(t *testing.T) {
	in := "[laughs] That is **funny** — really, [excited] truly."
	if got := speakableFor(in, true); got != "[laughs] That is funny, really, [excited] truly." {
		t.Errorf("tags on: %q", got)
	}
	if got := speakableFor(in, false); got != "That is funny, really, truly." {
		t.Errorf("tags off: %q", got)
	}
	if got := speakable("see [the docs] now"); strings.Contains(got, "[") {
		t.Errorf("brackets survived: %q", got)
	}
}

func TestStreamedTagsFollowTheCapabilityFlag(t *testing.T) {
	for _, tags := range []bool{true, false} {
		log := &sayLog{}
		sess := &streamSession{deltas: []string{"[sighs] Okay, that is done for you now."}}
		b := newTestBrain(sess, nil)
		b.tags = tags
		_, _ = b.Answer(voice.WithSay(context.Background(), log.say), voice.Utterance{Text: "x"})
		got := strings.Join(log.all(), " ")
		if tags != strings.Contains(got, "[sighs]") {
			t.Errorf("tags=%v spoken %q", tags, got)
		}
	}
	if !strings.Contains(callPrompt(true), "[laughs]") || strings.Contains(callPrompt(false), "[laughs]") {
		t.Error("tag guidance must appear only for tag voices")
	}
}
