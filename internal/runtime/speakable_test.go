package runtime

import "testing"

func TestSpeakableDropsIdentifiers(t *testing.T) {
	cases := map[string]string{
		"Siva is the CampX contact — his number is 209517111472259. You've got a meeting.": "Siva is the CampX contact. You've got a meeting.",
		"Reach him at 917671837092@s.whatsapp.net tomorrow":                                "Reach him at tomorrow",
		"See https://example.com/x for details":                                            "See for details",
		"**Bold** and a `code` word":                                                       "Bold and a code word",
	}
	for in, want := range cases {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
