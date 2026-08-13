package whatsapp

import "testing"

func TestIsAddressSkipsResolutionForRealAddresses(t *testing.T) {
	// A JID or phone number is already the answer; a round trip to resolve it is
	// only a new way for the send to fail.
	for _, ref := range []string{
		"39810169344106@lid",
		"917671837092@s.whatsapp.net",
		"120363424186016106@g.us",
		"917671837092",
		"+917671837092",
	} {
		if !isAddress(ref) {
			t.Errorf("%q should be treated as an address", ref)
		}
	}
}

func TestIsAddressSendsNamesToResolution(t *testing.T) {
	for _, ref := range []string{"Dev", "Siva Kumar", "god mode - The VC", "whatsapp-main", ""} {
		if isAddress(ref) {
			t.Errorf("%q should be resolved, not sent as-is", ref)
		}
	}
}
