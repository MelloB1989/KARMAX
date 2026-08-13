package comms

import (
	"errors"
	"fmt"
	"testing"
)

func TestUnresolvedTargetIsDistinguishableFromAChannelFault(t *testing.T) {
	// The send path decides whether to wake the operator on this distinction:
	// a bad address is the caller's to fix, a dead transport is not.
	var unresolved *UnresolvedTargetError
	bad := fmt.Errorf("sending: %w", &UnresolvedTargetError{
		Target: "whatsapp-main", Reason: `send: no matches found for "whatsapp-main"`,
	})
	if !errors.As(bad, &unresolved) {
		t.Fatal("an unresolved target must survive wrapping")
	}
	if unresolved.Target != "whatsapp-main" {
		t.Errorf("target = %q", unresolved.Target)
	}

	transportDown := fmt.Errorf("send whatsapp message: exit status 1")
	if errors.As(transportDown, &unresolved) {
		t.Error("a generic failure must not be mistaken for a bad address")
	}
}

func TestUnresolvedTargetErrorNamesTheTarget(t *testing.T) {
	e := &UnresolvedTargetError{Target: "Siva"}
	if got := e.Error(); got != "no such recipient: Siva" {
		t.Errorf("Error() = %q", got)
	}
}
