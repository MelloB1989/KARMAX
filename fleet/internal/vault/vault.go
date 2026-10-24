// Package vault seals the secrets the operator hands agents: AES-256-GCM
// under a key that lives in one 0600 file in fleetd's state dir, made the
// first time it is needed. The ledger holds only what Seal returns.
//
// Nothing here puts a value, or anything derived from one, in an error.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// KeyFile is the key's name in fleetd's state dir.
const KeyFile = "secrets.key"

// Vault seals and opens values with the key at Path.
type Vault struct {
	Path string

	mu   sync.Mutex
	aead cipher.AEAD
}

// New is the vault whose key is (or will be) at path.
func New(path string) *Vault { return &Vault{Path: path} }

// cipher loads the key, making it on first use. A key file others can read
// is refused, as is one of the wrong size: a guessed or truncated key would
// seal values nobody can open again.
func (v *Vault) cipher() (cipher.AEAD, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.aead != nil {
		return v.aead, nil
	}
	key, err := os.ReadFile(v.Path)
	if errors.Is(err, fs.ErrNotExist) {
		key, err = v.makeKey()
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(v.Path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (%v): chmod 600 it", v.Path, st.Mode().Perm())
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s is %d bytes, not a 32-byte key", v.Path, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if v.aead, err = cipher.NewGCM(block); err != nil {
		return nil, err
	}
	return v.aead, nil
}

// makeKey writes a new random key, refusing to replace one another fleetd
// made at the same moment.
func (v *Vault) makeKey() ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(v.Path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(v.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return os.ReadFile(v.Path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(v.Path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(v.Path)
		return nil, err
	}
	return key, nil
}

// Seal encrypts a value. The name is bound in as associated data, so a
// sealed value moved to another name does not open.
func (v *Vault) Seal(name string, value []byte) ([]byte, error) {
	a, err := v.cipher()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize(), a.NonceSize()+len(value)+a.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, value, []byte(name)), nil
}

// ErrOpen is a sealed value that does not open: the key changed, or the
// ledger row was altered.
var ErrOpen = errors.New("the sealed value does not open with this key")

// Open decrypts what Seal made for the same name.
func (v *Vault) Open(name string, sealed []byte) ([]byte, error) {
	a, err := v.cipher()
	if err != nil {
		return nil, err
	}
	if len(sealed) < a.NonceSize() {
		return nil, ErrOpen
	}
	out, err := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte(name))
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}
