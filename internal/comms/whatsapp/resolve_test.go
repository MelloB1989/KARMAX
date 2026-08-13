package whatsapp

import "testing"

func TestIsAddressSkipsResolutionForRealAddresses(t *testing.T) {
	// A JID or phone number is already the answer; a round trip to resolve it is
	// only a new way for the send to fail.
	for _, ref := range []string{
		"100000000000003@lid",
		"15555550101@s.whatsapp.net",
		"120000000000000001@g.us",
		"15555550101",
		"+15555550101",
	} {
		if !isAddress(ref) {
			t.Errorf("%q should be treated as an address", ref)
		}
	}
}

func TestIsAddressSendsNamesToResolution(t *testing.T) {
	for _, ref := range []string{"Dev", "Omer Kumar", "god mode - The VC", "whatsapp-main", ""} {
		if isAddress(ref) {
			t.Errorf("%q should be resolved, not sent as-is", ref)
		}
	}
}
