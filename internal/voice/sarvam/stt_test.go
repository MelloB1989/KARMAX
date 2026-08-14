package sarvam

import (
	"encoding/json"
	"testing"
)

// These are verbatim messages captured from the live socket. They exist because the shapes are not
// guessable and the first implementation got them wrong: it expected "speech_start"/"transcript"
// types with an is_final flag, and none of those exist.
const (
	rawStartSpeech = `{"type":"events","data":{"signal_type":"START_SPEECH","occured_at":1786568813.2485468}}`
	rawEndSpeech   = `{"type":"events","data":{"signal_type":"END_SPEECH","occured_at":1786568814.1554313}}`
	rawTranscript  = `{"type":"data","data":{"request_id":"20260812_daebc029","transcript":"मैंने आज सुबह अपनी दवाई ले ली है।","timestamps":null,"language_code":"hi-IN","metrics":{"audio_duration":3.04,"processing_latency":0.0915}}}`
	rawError       = `{"type":"error","data":{"message":"Error in Pipeline : validation error"}}`
)

func decode(t *testing.T, raw string) sttMessage {
	t.Helper()
	var m sttMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestVADSignalsMapToEvents(t *testing.T) {
	// START_SPEECH is what triggers barge-in; END_SPEECH is what starts a reply. Getting either
	// wrong makes the agent either talk over people or never answer them.
	ev, ok := decode(t, rawStartSpeech).event()
	if !ok || ev.Kind != SpeechStart {
		t.Fatalf("START_SPEECH mapped to %+v (ok=%v)", ev, ok)
	}
	ev, ok = decode(t, rawEndSpeech).event()
	if !ok || ev.Kind != SpeechEnd {
		t.Fatalf("END_SPEECH mapped to %+v (ok=%v)", ev, ok)
	}
}

func TestTranscriptIsExtractedAndFinal(t *testing.T) {
	ev, ok := decode(t, rawTranscript).event()
	if !ok {
		t.Fatal("transcript message produced no event")
	}
	if ev.Kind != Transcript {
		t.Fatalf("kind = %v, want Transcript", ev.Kind)
	}
	if ev.Text != "मैंने आज सुबह अपनी दवाई ले ली है।" {
		t.Fatalf("text = %q", ev.Text)
	}
	// A "data" message is a settled utterance; there is no partial/final flag on the wire.
	if !ev.Final {
		t.Fatal("transcripts from this socket are always final")
	}
}

func TestUnknownAndErrorMessagesAreIgnored(t *testing.T) {
	for name, raw := range map[string]string{
		"error":          rawError,
		"unknown type":   `{"type":"something_new","data":{}}`,
		"empty text":     `{"type":"data","data":{"transcript":""}}`,
		"unknown signal": `{"type":"events","data":{"signal_type":"MYSTERY"}}`,
	} {
		if _, ok := decode(t, raw).event(); ok {
			t.Fatalf("%s should not produce an event", name)
		}
	}
}

func TestAudioChunkShape(t *testing.T) {
	// The chunk nests under "audio", and the encoding label is "audio/wav" even though the bytes
	// are raw PCM — the socket rejects the honest "audio/x-raw" outright.
	b, err := json.Marshal(sttAudio{Audio: sttAudioBody{
		Data: "AAAA", Encoding: "audio/wav", SampleRate: SampleRate,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	audio, ok := got["audio"].(map[string]any)
	if !ok {
		t.Fatalf("audio must be nested, got %T", got["audio"])
	}
	if audio["encoding"] != "audio/wav" {
		t.Fatalf("encoding = %v, want audio/wav", audio["encoding"])
	}
	if audio["sample_rate"] != float64(SampleRate) {
		t.Fatalf("sample_rate = %v", audio["sample_rate"])
	}
}

func TestSplitSentencesBreaksOnDanda(t *testing.T) {
	got := SplitSentences("नमस्ते बेटा। आप कैसी हैं?")
	if len(got) != 2 || got[0] != "नमस्ते बेटा।" {
		t.Fatalf("got %q", got)
	}
}
