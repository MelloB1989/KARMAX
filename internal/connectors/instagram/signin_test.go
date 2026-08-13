package instagram

import (
	"encoding/base32"
	"errors"
	"strings"
	"testing"
)

func TestSignInErrorNamesAMissingTOTPSeed(t *testing.T) {
	// The exact failure an operator hit: 2FA is on, no seed was given, and the
	// library reported the symptom of decoding an empty string.
	raw := errors.New("Failed to generate 2FA OTP code: illegal base32 data at input byte 0")
	got := signInError(raw, false).Error()

	for _, want := range []string{"two-factor", "totp_seed", "base32"} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("missing-seed error should mention %q, got: %s", want, got)
		}
	}
	// The old blanket hint sent people to the app for a problem the app cannot fix.
	if strings.Contains(strings.ToLower(got), "flagged") {
		t.Errorf("must not blame a flagged account for a missing seed: %s", got)
	}
}

func TestSignInErrorDistinguishesABadSeedFromNoSeed(t *testing.T) {
	raw := errors.New("Failed to generate 2FA OTP code: illegal base32 data at input byte 3")
	got := strings.ToLower(signInError(raw, true).Error())

	if strings.Contains(got, "run `karmax login instagram` again and paste") {
		t.Error("a seed WAS supplied; telling them to supply one is wrong advice")
	}
	if !strings.Contains(got, "not usable") {
		t.Errorf("should say the seed itself was rejected, got: %s", got)
	}
}

func TestSignInErrorStillReportsARealChallenge(t *testing.T) {
	raw := errors.New("challenge_required")
	got := strings.ToLower(signInError(raw, false).Error())
	if !strings.Contains(got, "flagged") {
		t.Errorf("a genuine challenge should still say so, got: %s", got)
	}
}

func TestSignInErrorKeepsTheUnderlyingCause(t *testing.T) {
	raw := errors.New("some new failure nobody has seen")
	err := signInError(raw, false)
	if !errors.Is(err, raw) {
		t.Error("the original error must stay unwrappable")
	}
	if !strings.Contains(err.Error(), "some new failure") {
		t.Errorf("unclassified errors must pass through: %s", err)
	}
}

func TestNormalizeTOTPSeedAcceptsWhatInstagramShows(t *testing.T) {
	// Instagram displays the secret lowercase, in space-separated groups of
	// four, with no padding. Pasted exactly as shown it used to fail.
	got, err := normalizeTOTPSeed("abcd efgh ijkl mnop qrst uvwx yz23 4567")
	if err != nil {
		t.Fatalf("the displayed form must be accepted: %v", err)
	}
	if strings.ContainsAny(got, " \t") {
		t.Errorf("spaces survived: %q", got)
	}
	if got != strings.ToUpper(got) {
		t.Errorf("not uppercased: %q", got)
	}
	if _, err := base32.StdEncoding.DecodeString(got); err != nil {
		t.Errorf("result must decode with the same decoder goinsta uses: %v", err)
	}
}

func TestNormalizeTOTPSeedPadsShortSeeds(t *testing.T) {
	// 26 characters: valid base32 content, but StdEncoding refuses it unpadded.
	got, err := normalizeTOTPSeed("abcdefghijklmnopqrstuvwxyz")
	if err != nil {
		t.Fatalf("a 26-character seed should be padded, not rejected: %v", err)
	}
	if len(got)%8 != 0 {
		t.Errorf("length %d is not a multiple of 8: %q", len(got), got)
	}
	if _, err := base32.StdEncoding.DecodeString(got); err != nil {
		t.Errorf("padded seed still does not decode: %v", err)
	}
}

func TestNormalizeTOTPSeedNamesTheBadCharacter(t *testing.T) {
	// 0, 1, 8 and 9 are absent from the base32 alphabet and are exactly what
	// someone transcribing by eye gets wrong.
	_, err := normalizeTOTPSeed("ABC0EFGH")
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.Contains(err.Error(), "'0'") || !strings.Contains(err.Error(), "A-Z") {
		t.Errorf("error should name the character and the alphabet, got: %v", err)
	}
}

func TestNormalizeTOTPSeedRejectsASixDigitCode(t *testing.T) {
	// The likeliest mistake: pasting the rotating code instead of the secret.
	if _, err := normalizeTOTPSeed("123456"); err == nil {
		t.Error("a six-digit code is not a seed and must be refused")
	}
}

func TestNormalizeTOTPSeedToleratesPaddingAndSeparators(t *testing.T) {
	got, err := normalizeTOTPSeed("ABCD-EFGH_IJKL MNOP====")
	if err != nil {
		t.Fatalf("separators and existing padding should be tolerated: %v", err)
	}
	if _, err := base32.StdEncoding.DecodeString(got); err != nil {
		t.Errorf("result does not decode: %v", err)
	}
}

func TestNormalizeTOTPSeedRejectsAnEmptySeed(t *testing.T) {
	if _, err := normalizeTOTPSeed("   "); err == nil {
		t.Error("whitespace-only must be refused, not padded into nonsense")
	}
}
