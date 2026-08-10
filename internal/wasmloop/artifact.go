// Package wasmloop is the extension tier: third-party loops as signed WASM
// modules instead of Go compiled into the daemon.
//
// What it replaces: `karmax loops install` ran `go get`, rewrote a generated
// file, rebuilt the binary and swapped it. That needs a Go toolchain and a
// source checkout on every user's machine, and it compiles unreviewed
// third-party Go into a process holding WhatsApp sessions, Google tokens and a
// GitLoom API key. There is no boundary in that design to enforce.
//
// Install here is a TRANSACTION — fetch, verify, record, activate — and raw
// bytes are never executed on arrival.
package wasmloop

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// magic identifies a KARMAX loop artifact. Checked before anything is parsed,
// so pointing install at the wrong file says so instead of failing obscurely.
var magic = [8]byte{'K', 'M', 'X', 'L', 'O', 'O', 'P', 1}

const (
	// maxHeaderBytes bounds the manifest before it is decoded.
	maxHeaderBytes = 64 << 10
	// maxModuleBytes bounds the WASM. Generous — a Go-built guest is ~2MB —
	// but not unbounded, because the length comes from the file.
	maxModuleBytes = 64 << 20
)

// Manifest is what the artifact declares about itself. It is inside the
// signature, so none of it can be changed after publishing.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	// Publisher is the Ed25519 key that signed this artifact.
	Publisher string `json:"publisher"`

	// Host names the host functions the module may call. Anything not listed
	// is refused at runtime, not logged.
	Host []string `json:"host"`
	// Capabilities are Broker grants in class:value form — "http:api.github.com",
	// "memory:nexus:write", "tool:comms.send".
	Capabilities []string `json:"capabilities"`

	// Triggers.
	Schedule string   `json:"schedule,omitempty"`
	Events   []string `json:"events,omitempty"`
	Webhook  string   `json:"webhook,omitempty"`

	// Limits the operator can see before installing.
	MemoryMB int `json:"memory_mb,omitempty"`

	// SHA256 is the hex digest of the module bytes, inside the signature so the
	// code cannot be swapped for different code under the same manifest.
	SHA256 string `json:"sha256"`

	// Provenance.
	SourceURL string `json:"source_url,omitempty"`
	BuiltAt   int64  `json:"built_at,omitempty"`
}

// Role says which signature this is.
type Role string

const (
	RolePublisher Role = "publisher"
	RoleRegistry  Role = "registry"
)

// Signature is one attestation over the manifest.
type Signature struct {
	Role Role   `json:"role"`
	Key  string `json:"key"`
	Sig  string `json:"sig"`
}

// header is the artifact's JSON preamble.
type header struct {
	Manifest   Manifest    `json:"manifest"`
	Signatures []Signature `json:"signatures"`
}

// Artifact is a parsed loop package.
type Artifact struct {
	Manifest   Manifest
	Signatures []Signature
	Module     []byte
}

// signingString is what every signature covers.
//
// Built from fields rather than from the JSON as written, for the same reason
// the mesh signs canonical bytes: two encoders disagree about key order and
// whitespace, and a signature that verifies on one and not the other is worse
// than no signature.
func (m *Manifest) signingString() string {
	host := append([]string(nil), m.Host...)
	caps := append([]string(nil), m.Capabilities...)
	events := append([]string(nil), m.Events...)
	sort.Strings(host)
	sort.Strings(caps)
	sort.Strings(events)

	return strings.Join([]string{
		"karmax-loop-v1",
		"name=" + m.Name,
		"version=" + m.Version,
		"publisher=" + m.Publisher,
		"host=" + strings.Join(host, ","),
		"caps=" + strings.Join(caps, ","),
		"schedule=" + m.Schedule,
		"events=" + strings.Join(events, ","),
		"webhook=" + m.Webhook,
		"memory_mb=" + fmt.Sprint(m.MemoryMB),
		"sha256=" + strings.ToLower(m.SHA256),
		"source=" + m.SourceURL,
		"built=" + fmt.Sprint(m.BuiltAt),
	}, "|")
}

// Sign produces a signature over a manifest.
func Sign(m *Manifest, role Role, priv ed25519.PrivateKey) Signature {
	pub := priv.Public().(ed25519.PublicKey)
	return Signature{
		Role: role,
		Key:  base64.RawURLEncoding.EncodeToString(pub),
		Sig:  base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(m.signingString()))),
	}
}

// Pack builds an artifact file: magic, header length, header, module.
func Pack(m Manifest, module []byte, sigs []Signature) ([]byte, error) {
	if len(module) == 0 {
		return nil, fmt.Errorf("wasmloop: no module bytes")
	}
	if len(module) > maxModuleBytes {
		return nil, fmt.Errorf("wasmloop: module is %d bytes; the limit is %d", len(module), maxModuleBytes)
	}
	// The digest is recomputed rather than trusted from the caller: a manifest
	// whose sha256 does not describe the bytes beside it is the whole attack.
	sum := sha256.Sum256(module)
	m.SHA256 = hex.EncodeToString(sum[:])

	head, err := json.Marshal(header{Manifest: m, Signatures: sigs})
	if err != nil {
		return nil, err
	}
	if len(head) > maxHeaderBytes {
		return nil, fmt.Errorf("wasmloop: manifest is %d bytes; the limit is %d", len(head), maxHeaderBytes)
	}

	out := make([]byte, 0, len(magic)+4+len(head)+len(module))
	out = append(out, magic[:]...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(head)))
	out = append(out, head...)
	out = append(out, module...)
	return out, nil
}

// Unpack parses an artifact without verifying it. Verify does that, and nothing
// should execute a module that has only been unpacked.
func Unpack(data []byte) (*Artifact, error) {
	if len(data) < len(magic)+4 {
		return nil, fmt.Errorf("wasmloop: file is too short to be a loop artifact")
	}
	if string(data[:len(magic)]) != string(magic[:]) {
		return nil, fmt.Errorf("wasmloop: not a KARMAX loop artifact (bad magic)")
	}
	offset := len(magic)
	headLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
	offset += 4
	if headLen > maxHeaderBytes || offset+headLen > len(data) {
		return nil, fmt.Errorf("wasmloop: manifest length %d is not plausible", headLen)
	}

	var h header
	if err := json.Unmarshal(data[offset:offset+headLen], &h); err != nil {
		return nil, fmt.Errorf("wasmloop: manifest is unreadable: %w", err)
	}
	module := data[offset+headLen:]
	if len(module) == 0 {
		return nil, fmt.Errorf("wasmloop: artifact contains no module")
	}
	if len(module) > maxModuleBytes {
		return nil, fmt.Errorf("wasmloop: module is %d bytes; the limit is %d", len(module), maxModuleBytes)
	}
	return &Artifact{Manifest: h.Manifest, Signatures: h.Signatures, Module: module}, nil
}

// Digest is the hex sha256 of the module bytes.
func (a *Artifact) Digest() string {
	sum := sha256.Sum256(a.Module)
	return hex.EncodeToString(sum[:])
}

// BuiltAtTime renders the build timestamp.
func (m *Manifest) BuiltAtTime() time.Time {
	if m.BuiltAt == 0 {
		return time.Time{}
	}
	return time.Unix(m.BuiltAt, 0)
}
