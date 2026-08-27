package store

import (
	"testing"
	"time"
)

func TestOneConnectorHoldsACredentialPerEmployee(t *testing.T) {
	s := newTestStore(t)

	for _, m := range []string{"kartik", "priya"} {
		if err := s.SaveUserCredential(UserCredential{
			Connector: "google", Member: m, Account: m + "@acme.com",
			AccessToken: "at-" + m, RefreshToken: "rt-" + m,
			Scopes: []string{"gmail.readonly", "calendar"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Reading Priya's calendar with Kartik's token is not a permissions bug to
	// tighten later; it is the wrong answer returned confidently.
	k, err := s.UserCredential("google", "kartik")
	if err != nil || k == nil {
		t.Fatal(err)
	}
	if k.AccessToken != "at-kartik" || k.Account != "kartik@acme.com" {
		t.Errorf("got the wrong employee's credential: %+v", k)
	}
	if len(k.Scopes) != 2 || k.Scopes[0] != "gmail.readonly" {
		t.Errorf("scopes did not round-trip: %v", k.Scopes)
	}

	missing, err := s.UserCredential("google", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Error("an employee who never connected returned a credential")
	}
}

// Google returns a refresh token on the FIRST consent only. Overwriting a
// stored one with "" on a later re-consent silently converts a durable
// connection into one that dies in an hour.
func TestReconsentDoesNotWipeTheRefreshToken(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveUserCredential(UserCredential{
		Connector: "google", Member: "kartik", AccessToken: "at-1", RefreshToken: "rt-1",
	}); err != nil {
		t.Fatal(err)
	}
	// A refresh, or a re-consent where Google omits the refresh token.
	if err := s.SaveUserCredential(UserCredential{
		Connector: "google", Member: "kartik", AccessToken: "at-2", RefreshToken: "",
	}); err != nil {
		t.Fatal(err)
	}

	c, _ := s.UserCredential("google", "kartik")
	if c.AccessToken != "at-2" {
		t.Errorf("the access token did not update: %q", c.AccessToken)
	}
	if c.RefreshToken != "rt-1" {
		t.Errorf("the refresh token was wiped by a re-consent: %q — the connection would "+
			"have silently died in an hour", c.RefreshToken)
	}

	// An explicitly supplied new refresh token must still replace it.
	if err := s.SaveUserCredential(UserCredential{
		Connector: "google", Member: "kartik", AccessToken: "at-3", RefreshToken: "rt-2",
	}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.UserCredential("google", "kartik")
	if c.RefreshToken != "rt-2" {
		t.Errorf("a real new refresh token was ignored: %q", c.RefreshToken)
	}
}

// A token that dies mid-request produces a 401 that reads like a revoked grant
// rather than an expiry, sending whoever debugs it to the wrong place.
func TestExpiryIsTreatedAsEarly(t *testing.T) {
	soon := time.Now().Add(30 * time.Second)
	if !(UserCredential{ExpiresAt: &soon}).Expired() {
		t.Error("a token expiring in 30s should already count as expired")
	}
	later := time.Now().Add(30 * time.Minute)
	if (UserCredential{ExpiresAt: &later}).Expired() {
		t.Error("a token good for 30 minutes was treated as expired")
	}
	if (UserCredential{}).Expired() {
		t.Error("a credential with no expiry should not be expired")
	}
}

// The console's "who has connected" list only needs names.
func TestListingConnectionsDoesNotHandOutTokens(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveUserCredential(UserCredential{
		Connector: "google", Member: "kartik", Account: "k@acme.com",
		AccessToken: "at-secret", RefreshToken: "rt-secret",
	}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListUserCredentials("google")
	if err != nil || len(list) != 1 {
		t.Fatalf("got %d rows (%v)", len(list), err)
	}
	if list[0].AccessToken != "" || list[0].RefreshToken != "" {
		t.Error("the listing returned tokens — a screen showing names should not carry them")
	}
	if list[0].Account != "k@acme.com" {
		t.Errorf("the account is what the screen needs, got %q", list[0].Account)
	}
}

func TestDisconnectingRemovesOnlyThatEmployee(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []string{"kartik", "priya"} {
		if err := s.SaveUserCredential(UserCredential{Connector: "google", Member: m, AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteUserCredential("google", "kartik"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.UserCredential("google", "kartik"); c != nil {
		t.Error("the disconnected employee still has a credential")
	}
	if c, _ := s.UserCredential("google", "priya"); c == nil {
		t.Error("disconnecting one employee removed another's")
	}
}

// A state token is a single-use credential. Leaving a redeemed one behind is
// what makes a leaked callback URL replayable.
func TestAnOAuthStateWorksExactlyOnce(t *testing.T) {
	s := newTestStore(t)

	state, err := s.CreateOAuthState("google", "kartik", "verifier", "/settings", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.RedeemOAuthState(state)
	if err != nil {
		t.Fatal(err)
	}
	if got.Member != "kartik" || got.Verifier != "verifier" {
		t.Errorf("state did not round-trip: %+v", got)
	}

	if _, err := s.RedeemOAuthState(state); err == nil {
		t.Error("the same state was redeemed twice — a leaked callback URL would be replayable")
	}
	if _, err := s.RedeemOAuthState("never-existed"); err == nil {
		t.Error("an unknown state was accepted")
	}
}

func TestAnExpiredOAuthStateIsRefusedAndConsumed(t *testing.T) {
	s := newTestStore(t)

	state, err := s.CreateOAuthState("google", "kartik", "v", "", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemOAuthState(state); err == nil {
		t.Error("an expired state was accepted")
	}
	// Consumed anyway: an expired row left behind is still a row to replay.
	if _, err := s.RedeemOAuthState(state); err == nil {
		t.Error("the expired state survived redemption")
	}
}
