package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const cookieName = "fc"

// ReadTokenFile reads a token file and refuses one that anyone but its owner
// can read.
func ReadTokenFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %04o: a token file must be 0600", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

type authn struct{ token string }

// equal compares in constant time regardless of length.
func (a authn) equal(got string) bool {
	x, y := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(a.token))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// ok: the bearer header or the login cookie carries the token.
func (a authn) ok(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && a.equal(strings.TrimPrefix(h, "Bearer ")) {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && a.equal(c.Value) {
		return true
	}
	return false
}

// login sets the cookie and redirects to / so the token does not stay in the
// URL.
func (a authn) login(w http.ResponseWriter, r *http.Request) {
	if !a.equal(r.URL.Query().Get("k")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
