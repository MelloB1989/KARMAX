package runtime

import "testing"

func TestSpeakableDropsIdentifiers(t *testing.T) {
	cases := map[string]string{
		"Omer is the Contoso contact — his number is 100000000000008. You've got a meeting.": "Omer is the Contoso contact. You've got a meeting.",
		"Reach him at 15555550101@s.whatsapp.net tomorrow":                                "Reach him at tomorrow",
		"See https://example.com/x for details":                                            "See for details",
		"**Bold** and a `code` word":                                                       "Bold and a code word",
	}
	for in, want := range cases {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
