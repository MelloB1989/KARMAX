package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// FilesOps is fleetd's machine's files (internal/hostfiles), for the Files
// tab. Every route to it is the full scope's alone.
type FilesOps interface {
	List(path string) (fleetapi.FileDir, error)
	Open(path string) (fs.File, fs.FileInfo, error)
	Preview(ctx context.Context, path string, w, h int) ([]byte, string, error)
	Tar(ctx context.Context, path string, gz bool, w io.Writer) error
	Abs(path string) (string, error)
	Put(dir, name, id string, off, size int64, body io.Reader) (fleetapi.FilePut, error)
	Have(id string) (int64, error)
	Untar(dir string, body io.Reader) (fleetapi.FilePut, error)
	Op(op fleetapi.FileOp) (fleetapi.FileOpDone, error)
}

// HostFiles is a hostfiles.Manager as FilesOps.
func HostFiles(m *hostfiles.Manager) FilesOps { return hostFiles{m} }

type hostFiles struct{ *hostfiles.Manager }

func (h hostFiles) Open(path string) (fs.File, fs.FileInfo, error) {
	f, fi, err := h.Manager.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, fi, nil
}

// WithFiles is ops with the files: the API serves them, and lan (when not
// nil) serves their bulk routes on the local network too.
func WithFiles(ops Ops, f FilesOps, lan *LAN) Ops { return withFiles{Ops: ops, files: f, lan: lan} }

type withFiles struct {
	Ops
	files FilesOps
	lan   *LAN
}

func filesOf(ops Ops) (FilesOps, *LAN) {
	for {
		switch w := ops.(type) {
		case withFiles:
			return w.files, w.lan
		case withDesk:
			ops = w.Ops
		default:
			return nil, nil
		}
	}
}

// filesHandler is one files route: its answer, or an error, or streamed.
type filesHandler func(w http.ResponseWriter, r *http.Request) (any, error)

// filesRoutes is the files' routes, bulk ones (what moves bytes) apart
// from the rest, which only the API serves.
func filesRoutes(f FilesOps) (bulk, rest map[string]filesHandler) {
	bulk, rest = map[string]filesHandler{}, map[string]filesHandler{}
	rest["GET "+fleetapi.PathFiles] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		l, err := f.List(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		return gzipped(w, r, l)
	}
	rest["POST "+fleetapi.PathFilesOp] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		var op fleetapi.FileOp
		if err := body(r, &op); err != nil {
			return nil, err
		}
		return f.Op(op)
	}
	bulk["GET "+fleetapi.PathFilesRead] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		file, fi, err := f.Open(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		defer file.Close()
		rs, ok := file.(io.ReadSeeker)
		if !ok {
			return nil, errors.New("the file cannot seek")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fi.Name()}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "", fi.ModTime(), rs)
		return streamed{}, nil
	}
	bulk["GET "+fleetapi.PathFilesPreview] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		pw, _ := strconv.Atoi(q.Get("w"))
		ph, _ := strconv.Atoi(q.Get("h"))
		b, typ, err := f.Preview(r.Context(), q.Get("path"), pw, ph)
		if err != nil {
			return nil, err
		}
		w.Header().Set("Content-Type", typ)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
		return streamed{}, nil
	}
	bulk["GET "+fleetapi.PathFilesTar] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		abs, err := f.Abs(q.Get("path"))
		if err != nil {
			return nil, err
		}
		if _, err := f.List(abs); err != nil { // there, and readable: an error before the stream starts
			return nil, err
		}
		if n := hostfiles.Count(abs, 1500*time.Millisecond); n >= 0 {
			w.Header().Set("X-Fleet-Bytes", strconv.FormatInt(n, 10))
		}
		w.Header().Set("Cache-Control", "no-store")
		sw := &startWriter{w: w, contentType: "application/x-tar"}
		if err := f.Tar(r.Context(), abs, q.Get("gz") == "1", sw); err != nil && !sw.started {
			return nil, err
		}
		return streamed{}, nil // a failure midway cuts the stream: the tar has no end
	}
	bulk["PUT "+fleetapi.PathFilesPut] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		off, err := intParam(r, "offset")
		if err != nil {
			return nil, err
		}
		size, err := intParam(r, "size")
		if err != nil {
			return nil, err
		}
		return f.Put(q.Get("dir"), q.Get("name"), q.Get("id"), off, size, r.Body)
	}
	bulk["GET "+fleetapi.PathFilesPut] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, err := f.Have(r.URL.Query().Get("id"))
		return fleetapi.FilePut{Have: n}, err
	}
	bulk["PUT "+fleetapi.PathFilesUntar] = func(w http.ResponseWriter, r *http.Request) (any, error) {
		return f.Untar(r.URL.Query().Get("dir"), r.Body)
	}
	return bulk, rest
}

// gzipped writes v as JSON, gzipped when the caller takes it and it is
// big: a folder of thousands of entries, on a slow link.
func gzipped(w http.ResponseWriter, r *http.Request, v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	w.Header().Set("Content-Type", "application/json")
	if len(b) < 8<<10 || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		_, _ = w.Write(append(b, '\n'))
		return streamed{}, nil
	}
	var z bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&z, gzip.BestSpeed)
	_, _ = zw.Write(b)
	_ = zw.Close()
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Length", strconv.Itoa(z.Len()))
	_, _ = w.Write(z.Bytes())
	return streamed{}, nil
}

// filesErr is a files error as the API answers it.
func filesErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hostfiles.ErrNotFound):
		return notFound{err}
	case errors.Is(err, hostfiles.ErrInvalid):
		return badRequest{err}
	case errors.Is(err, hostfiles.ErrDenied):
		return forbidden{err}
	case errors.Is(err, hostfiles.ErrExists), errors.Is(err, hostfiles.ErrBusy), errors.Is(err, hostfiles.ErrMismatch):
		return conflict{err}
	case errors.Is(err, hostfiles.ErrTooLarge):
		return tooLarge{err}
	}
	return err
}

// codeOf is the status an error from a handler is answered with, outside
// the API's own wrapper (the LAN's).
func codeOf(err error) int {
	switch err.(type) {
	case notFound:
		return http.StatusNotFound
	case badRequest:
		return http.StatusBadRequest
	case forbidden:
		return http.StatusForbidden
	case conflict:
		return http.StatusConflict
	case tooLarge:
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

// fileRoutes registers the files on the API.
func fileRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), ops Ops) {
	f, lan := filesOf(ops)
	off := func(w http.ResponseWriter, r *http.Request) (any, error) {
		return nil, notFound{errors.New("fleetd serves no files (fleet.yaml host_files.enabled)")}
	}
	bulk, rest := map[string]filesHandler{}, map[string]filesHandler{}
	if f != nil {
		bulk, rest = filesRoutes(f)
	}
	for _, p := range []string{"GET " + fleetapi.PathFiles, "POST " + fleetapi.PathFilesOp, "GET " + fleetapi.PathFilesRead,
		"GET " + fleetapi.PathFilesPreview, "GET " + fleetapi.PathFilesTar, "PUT " + fleetapi.PathFilesPut,
		"GET " + fleetapi.PathFilesPut, "PUT " + fleetapi.PathFilesUntar} {
		h := bulk[p]
		if h == nil {
			h = rest[p]
		}
		if h == nil {
			h = off
		}
		route(p, func(w http.ResponseWriter, r *http.Request) (any, error) {
			v, err := h(w, r)
			return v, filesErr(err)
		}, fullOnly...)
	}
	route("GET "+fleetapi.PathFilesLAN, func(w http.ResponseWriter, r *http.Request) (any, error) {
		if f == nil || lan == nil {
			return nil, notFound{errors.New("fleetd serves no files on its local network (fleet.yaml host_files.lan_listen)")}
		}
		return lan.issue(), nil
	}, fullOnly...)
}

// LAN is the files' bulk routes on fleetd's local network: HTTPS with a
// certificate made at start, whose hash the API hands out with a token.
// Only a caller the API let in (the full scope: an admin device) has the
// token; the certificate's hash, which it checks, came the same way, so the
// local network sees neither the files nor the token.
type LAN struct {
	// Port is the port it listens on, on every address.
	Port string
	// Addrs is this machine's addresses on its local networks; nil reads
	// the interfaces.
	Addrs func() []string
	// TokenFor is how long a token lasts.
	TokenFor time.Duration

	cert   tls.Certificate
	pin    string
	mu     sync.Mutex
	tokens map[string]time.Time
}

// NewLAN makes the LAN's certificate, for addresses on port.
func NewLAN(port string) (*LAN, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "fleetd files"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &LAN{Port: port, TokenFor: 12 * time.Hour, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pin: hex.EncodeToString(sum[:]), tokens: map[string]time.Time{}}, nil
}

// TLS is the LAN listener's TLS config.
func (l *LAN) TLS() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{l.cert}, MinVersion: tls.VersionTLS13}
}

func (l *LAN) issue() fleetapi.FileLAN {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	now := time.Now()
	exp := now.Add(l.TokenFor)
	l.mu.Lock()
	for t, e := range l.tokens {
		if now.After(e) {
			delete(l.tokens, t)
		}
	}
	l.tokens[tok] = exp
	l.mu.Unlock()
	addrs := l.Addrs
	if addrs == nil {
		addrs = LANAddrs
	}
	var out []string
	for _, a := range addrs() {
		out = append(out, net.JoinHostPort(a, l.Port))
	}
	return fleetapi.FileLAN{Addrs: out, Pin: l.pin, Token: tok, Expires: exp}
}

func (l *LAN) valid(tok string) bool {
	if tok == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for t, e := range l.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return now.Before(e)
		}
	}
	return false
}

// Handler is the LAN's routes: the files' bulk ones, for a token's holder.
func (l *LAN) Handler(ops Ops) http.Handler {
	mux := http.NewServeMux()
	f, _ := filesOf(ops)
	if f == nil {
		return mux
	}
	bulk, _ := filesRoutes(f)
	for p, h := range bulk {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if crossSite(r) {
				writeErr(w, http.StatusForbidden, errors.New("a request from a web page: fleetd's files take none"))
				return
			}
			if !l.valid(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
				writeErr(w, http.StatusUnauthorized, errors.New("a token from "+fleetapi.PathFilesLAN+" is required"))
				return
			}
			v, err := h(w, r)
			if _, done := v.(streamed); done && err == nil {
				return
			}
			if err = filesErr(err); err != nil {
				writeErr(w, codeOf(err), err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		})
	}
	return mux
}

// LANAddrs is this machine's IPv4 addresses on private networks it shares
// with others: not loopback, the tailnet, or a bridge for containers.
func LANAddrs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var wired, rest []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIf(ifc.Name) {
			continue
		}
		as, _ := ifc.Addrs()
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			if strings.HasPrefix(ifc.Name, "e") { // eth0, enp3s0: the wire first
				wired = append(wired, ipn.IP.String())
			} else {
				rest = append(rest, ipn.IP.String())
			}
		}
	}
	return append(wired, rest...)
}

func virtualIf(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "lxc", "tailscale", "utun", "tun", "wg", "zt", "fleet-", "cni", "flannel", "vmnet", "vboxnet"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
