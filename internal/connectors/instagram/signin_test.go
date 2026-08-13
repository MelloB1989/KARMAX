package instagram

import (
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
