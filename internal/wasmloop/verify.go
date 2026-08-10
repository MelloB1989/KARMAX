package wasmloop

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Verification.
//
// Every check here runs BEFORE the bytes reach wazero. An artifact that fails
// any of them is never compiled, never instantiated, and never on disk as an
// active loop.

// Tier is how much an artifact was vouched for.
type Tier string

const (
	// TierRegistry is countersigned by a registry key the operator trusts,
	// which means it went through review.
	TierRegistry Tier = "registry"
	// TierCommunity carries only a publisher signature. It runs, loudly, with
	// reduced defaults.
	TierCommunity Tier = "community"
	// TierUntrusted carries no valid signature at all. Nothing is known about
	// where it came from — only that the bytes match the manifest describing
	// them. It exists so a developer can run what they just built without
	// generating a key first, and it is never reached by accident.
	TierUntrusted Tier = "untrusted"
)

// Trust is the operator's configuration for what to accept.
type Trust struct {
	// Registries are the countersigning keys this instance trusts. Empty means
	// no artifact can reach registry tier.
	Registries []string
	// Revoked lists publisher or registry keys that must be refused, whatever
	// else is true of the artifact.
	Revoked []string
	// AllowCommunity permits publisher-only artifacts. Off by default: running
	// unreviewed code from a stranger should be a decision, not a default.
	AllowCommunity bool
	// AllowUntrusted accepts an artifact with no valid signature.
	//
	// Deliberately NOT a setting. It is set for ONE install, by an operator who
	// typed the loop's name to confirm, and it is not written to trust.json —
	// which is the difference between "I am running the thing I just built" and
	// "this machine now accepts anything". The global equivalent would be a
	// switch nobody remembers flipping.
	AllowUntrusted bool
}

// Verdict is the result of verifying an artifact.
type Verdict struct {
	Tier Tier
	// Publisher is the key that signed it.
	Publisher string
	// Registry is the countersigning key, when there is one.
	Registry string
}

// Verify checks an artifact end to end and reports what it may be trusted as.
func Verify(a *Artifact, t Trust) (*Verdict, error) {
	m := &a.Manifest

	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" {
		return nil, fmt.Errorf("wasmloop: the manifest has no name or version")
	}
	if strings.ContainsAny(m.Name, "/\\ ") {
		return nil, fmt.Errorf("wasmloop: %q is not a usable loop name", m.Name)
	}

	// The digest first: if the bytes are not the bytes the manifest describes,
	// nothing about the signatures matters.
	want := strings.ToLower(strings.TrimSpace(m.SHA256))
	if _, err := hex.DecodeString(want); err != nil || len(want) != 64 {
		return nil, fmt.Errorf("wasmloop: the manifest carries no usable sha256")
	}
	if got := a.Digest(); got != want {
		return nil, fmt.Errorf("wasmloop: the module does not match its manifest\n  manifest says %s\n  bytes are     %s", want, got)
	}

	// An artifact with no publisher at all is the unsigned case, decided below
	// once we know whether the operator asked for it. Anything that DOES name a
	// publisher must name a usable one — a malformed key is a broken artifact,
	// not an unsigned one, and must not quietly fall through to the softer path.
	if strings.TrimSpace(m.Publisher) != "" {
		if _, err := decodeKey(m.Publisher); err != nil {
			return nil, fmt.Errorf("wasmloop: publisher key: %w", err)
		}
	} else if len(a.Signatures) > 0 {
		return nil, fmt.Errorf("wasmloop: the artifact carries signatures but names no publisher")
	}

	revoked := map[string]bool{}
	for _, k := range t.Revoked {
		revoked[strings.TrimSpace(k)] = true
	}
	if revoked[m.Publisher] {
		return nil, fmt.Errorf("wasmloop: the publisher key is revoked")
	}

	signed := []byte(m.signingString())
	var havePublisher bool
	var registryKey string

	for _, s := range a.Signatures {
		key, err := decodeKey(s.Key)
		if err != nil {
			return nil, fmt.Errorf("wasmloop: %s signature key: %w", s.Role, err)
		}
		sig, err := base64.RawURLEncoding.DecodeString(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			return nil, fmt.Errorf("wasmloop: the %s signature is malformed", s.Role)
		}
		if !ed25519.Verify(key, signed, sig) {
			return nil, fmt.Errorf("wasmloop: the %s signature does not verify", s.Role)
		}
		if revoked[s.Key] {
			return nil, fmt.Errorf("wasmloop: the %s key is revoked", s.Role)
		}

		switch s.Role {
		case RolePublisher:
			// Must be the key the manifest names, or a publisher could sign
			// somebody else's artifact and have it count.
			if subtle.ConstantTimeCompare([]byte(s.Key), []byte(m.Publisher)) != 1 {
				return nil, fmt.Errorf("wasmloop: the publisher signature is from a different key than the manifest names")
			}
			havePublisher = true
		case RoleRegistry:
			if trusts(t.Registries, s.Key) {
				registryKey = s.Key
			}
		}
	}

	if !havePublisher {
		// No signature at all. The digest check above still ran, so the bytes
		// are the bytes this manifest describes — but nothing says who wrote
		// them, so this needs a per-install decision and never a default.
		if !t.AllowUntrusted {
			return nil, fmt.Errorf("wasmloop: %s is not signed by its publisher\n"+
				"  sign it with `karmax wloop sign`, or install it unsigned with --untrusted", m.Name)
		}
		return &Verdict{Tier: TierUntrusted}, nil
	}
	if registryKey != "" {
		return &Verdict{Tier: TierRegistry, Publisher: m.Publisher, Registry: registryKey}, nil
	}
	if !t.AllowCommunity && !t.AllowUntrusted {
		return nil, fmt.Errorf("wasmloop: %s is signed by its publisher but not countersigned by a registry this instance trusts\n"+
			"  install it anyway with --untrusted if you know who %s is,\n"+
			"  or accept every publisher-signed loop with `karmax wloop trust --allow-community`",
			m.Name, short(m.Publisher))
	}
	return &Verdict{Tier: TierCommunity, Publisher: m.Publisher}, nil
}

// VerifyBytes is Unpack plus Verify, which is the only correct order and so is
// the only one offered as a single call.
func VerifyBytes(data []byte, t Trust) (*Artifact, *Verdict, error) {
	a, err := Unpack(data)
	if err != nil {
		return nil, nil, err
	}
	v, err := Verify(a, t)
	if err != nil {
		return nil, nil, err
	}
	return a, v, nil
}

func trusts(keys []string, key string) bool {
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(k)), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func decodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("bad encoding: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
