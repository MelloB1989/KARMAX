package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// The Files tab's calls. Listings and changes go to the API; what moves
// bytes (reads, pictures, tars, uploads) goes over fleetd's local network
// when this machine is on it: fleetd hands out its LAN addresses, its
// certificate's hash and a token over the tailnet, and the first address
// that answers with that certificate takes the bytes, off the tailnet. One
// that stops answering (the laptop left home) sends them back to the
// tailnet at once, and the LAN is looked for again a little later.

// How the LAN is looked for.
const (
	lanProbe     = 900 * time.Millisecond // an address that takes longer is not near
	lanLookAgain = 2 * time.Minute        // after none answered, or one failed
)

// lanRoute is the LAN as last found.
type lanRoute struct {
	mu      sync.Mutex
	base    string // https://addr, "" for none
	token   string
	expires time.Time
	hc      *http.Client
	looked  time.Time
	looking chan struct{} // closed when a look in flight ends
}

var lanState lanRoute

// sameMachine: fleetd runs here, so its files are this machine's (a test
// says otherwise, to reach a fleetd on loopback as a remote one).
var sameMachine = onFleetdsMachine

// bulkTarget is where a bulk call goes: its base URL, bearer, client and
// route.
type bulkTarget struct {
	base, token, route string
	hc                 *http.Client
}

// bulk is where bytes go now: on fleetd's own machine, its API; else the
// LAN when it answers, else the tailnet.
func (a *apiClient) bulk(ctx context.Context) bulkTarget {
	api := bulkTarget{base: a.c.base, token: a.c.token, route: fleetapi.RouteTailnet, hc: deskHTTP()}
	if sameMachine(a.c.base) {
		api.route = fleetapi.RouteLocal
		return api
	}
	if os.Getenv("FLEET_FILES_LAN") == "off" {
		return api
	}
	l := &lanState
	l.mu.Lock()
	if l.base != "" && time.Now().Before(l.expires.Add(-time.Minute)) {
		t := bulkTarget{base: l.base, token: l.token, route: fleetapi.RouteLAN, hc: l.hc}
		l.mu.Unlock()
		return t
	}
	if wait := l.looking; wait != nil { // another call is looking: wait for it
		l.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return api
		}
		return a.bulkKnown(api)
	}
	if !l.looked.IsZero() && time.Since(l.looked) < lanLookAgain {
		l.mu.Unlock()
		return api
	}
	done := make(chan struct{})
	l.looking, l.base = done, ""
	l.mu.Unlock()
	base, tok, exp, hc := a.lookLAN(ctx)
	l.mu.Lock()
	l.base, l.token, l.expires, l.hc, l.looked, l.looking = base, tok, exp, hc, time.Now(), nil
	l.mu.Unlock()
	close(done)
	return a.bulkKnown(api)
}

func (a *apiClient) bulkKnown(api bulkTarget) bulkTarget {
	l := &lanState
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.base != "" {
		return bulkTarget{base: l.base, token: l.token, route: fleetapi.RouteLAN, hc: l.hc}
	}
	return api
}

// lanDown: a call over the LAN failed on the way, not with an answer; the
// next go to the tailnet.
func lanDown(base string) {
	l := &lanState
	l.mu.Lock()
	if l.base == base {
		l.base, l.looked = "", time.Now()
	}
	l.mu.Unlock()
}

// lookLAN asks fleetd for its LAN and tries every address at once; the
// first that shows fleetd's certificate and takes the token wins.
func (a *apiClient) lookLAN(ctx context.Context) (base, token string, exp time.Time, hc *http.Client) {
	var l fleetapi.FileLAN
	if err := a.filesDo(ctx, "GET", fleetapi.PathFilesLAN, nil, &l, 10*time.Second); err != nil || len(l.Addrs) == 0 {
		return "", "", time.Time{}, nil
	}
	pin, err := hex.DecodeString(l.Pin)
	if err != nil || len(pin) != sha256.Size {
		return "", "", time.Time{}, nil
	}
	tc := &tls.Config{
		InsecureSkipVerify: true, // the certificate is fleetd's own: checked by its hash, below
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !bytes.Equal(sum[:], pin) {
				return errors.New("not fleetd's certificate")
			}
			return nil
		},
	}
	hc = &http.Client{Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     tc,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 8, IdleConnTimeout: 2 * time.Minute,
		WriteBufferSize: 256 << 10, ReadBufferSize: 256 << 10,
	}}
	pctx, cancel := context.WithTimeout(ctx, lanProbe)
	defer cancel()
	won := make(chan string, len(l.Addrs))
	for _, addr := range l.Addrs {
		go func() {
			b := "https://" + addr
			req, _ := http.NewRequestWithContext(pctx, "GET", b+fleetapi.PathFilesPut+"?id=lanprobe00", nil)
			req.Header.Set("Authorization", "Bearer "+l.Token)
			resp, err := hc.Do(req)
			if err != nil {
				won <- ""
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				won <- ""
				return
			}
			won <- b
		}()
	}
	for range l.Addrs {
		if b := <-won; b != "" {
			return b, l.Token, l.Expires, hc
		}
	}
	return "", "", time.Time{}, nil
}

// filesDo is a JSON call to the API on the kept transport.
func (a *apiClient) filesDo(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.c.base+path, body)
	if err != nil {
		return err
	}
	if a.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := deskHTTP().Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("fleetd at %s did not answer in time", a.c.base)
		}
		return fmt.Errorf("fleetd unreachable at %s: %w", a.c.base, err)
	}
	defer resp.Body.Close()
	if err := statusOf(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusOf is a failing response as an error.
func statusOf(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct{ Error string }
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return &statusError{Code: resp.StatusCode, msg: e.Error, Body: b}
	}
	return &statusError{Code: resp.StatusCode, msg: "fleetd: " + resp.Status, Body: b}
}

// send is a bulk request: on the LAN when it is there, and a failure on
// the way there marks it down. A body that can be sent again (none) is
// retried on the tailnet at once.
func (a *apiClient) send(ctx context.Context, method, path string, body io.Reader, size int64, hdr http.Header) (*http.Response, string, error) {
	t := a.bulk(ctx)
	for {
		req, err := http.NewRequestWithContext(ctx, method, t.base+path, body)
		if err != nil {
			return nil, "", err
		}
		if body != nil {
			req.ContentLength = size
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		if t.token != "" {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
		resp, err := t.hc.Do(req)
		if err == nil {
			if err := statusOf(resp); err != nil {
				resp.Body.Close()
				return nil, t.route, err
			}
			return resp, t.route, nil
		}
		if t.route != fleetapi.RouteLAN || ctx.Err() != nil {
			return nil, t.route, fmt.Errorf("fleetd unreachable at %s: %w", t.base, err)
		}
		lanDown(t.base)
		if body != nil {
			return nil, t.route, fmt.Errorf("the local network: %w", err)
		}
		t = bulkTarget{base: a.c.base, token: a.c.token, route: fleetapi.RouteTailnet, hc: deskHTTP()}
	}
}

func (a *apiClient) FilesRoute() string {
	if sameMachine(a.c.base) {
		return fleetapi.RouteLocal
	}
	return a.bulkKnown(bulkTarget{route: fleetapi.RouteTailnet}).route
}

func (a *apiClient) FilesList(ctx context.Context, path string) (*fleetapi.FileDir, error) {
	var d fleetapi.FileDir
	if err := a.filesDo(ctx, "GET", fleetapi.PathFiles+"?path="+url.QueryEscape(path), nil, &d, callTimeout); err != nil {
		return nil, err
	}
	return &d, nil
}

func (a *apiClient) FilesOp(ctx context.Context, op fleetapi.FileOp) (*fleetapi.FileOpDone, error) {
	var d fleetapi.FileOpDone
	if err := a.filesDo(ctx, "POST", fleetapi.PathFilesOp, op, &d, 5*time.Minute); err != nil {
		return nil, err
	}
	return &d, nil
}

func (a *apiClient) FilesRead(ctx context.Context, path string, off, n int64, ifMod time.Time) (*fleetapi.FileRead, error) {
	h := http.Header{}
	switch {
	case n >= 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+max(n, 1)-1))
	case off > 0:
		h.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	if !ifMod.IsZero() && h.Get("Range") != "" {
		h.Set("If-Range", ifMod.UTC().Format(http.TimeFormat))
	}
	resp, route, err := a.send(ctx, "GET", fleetapi.PathFilesRead+"?path="+url.QueryEscape(path), nil, 0, h)
	var se *statusError
	if errors.As(err, &se) && se.Code == http.StatusRequestedRangeNotSatisfiable { // asked from its end: nothing more
		return &fleetapi.FileRead{Body: io.NopCloser(strings.NewReader("")), From: off, Size: off, Route: route}, nil
	}
	if err != nil {
		return nil, err
	}
	out := &fleetapi.FileRead{Body: resp.Body, Size: resp.ContentLength, Route: route}
	out.Mod, _ = http.ParseTime(resp.Header.Get("Last-Modified"))
	if resp.StatusCode == http.StatusPartialContent {
		var a, b, total int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &a, &b, &total); err == nil {
			out.From, out.Size = a, total
		}
	}
	return out, nil
}

func (a *apiClient) FilesPreview(ctx context.Context, path string, w, h int) ([]byte, error) {
	q := url.Values{"path": {path}, "w": {strconv.Itoa(w)}, "h": {strconv.Itoa(h)}}
	resp, _, err := a.send(ctx, "GET", fleetapi.PathFilesPreview+"?"+q.Encode(), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func (a *apiClient) FilesTar(ctx context.Context, path string) (*fleetapi.FileRead, error) {
	q := url.Values{"path": {path}}
	if t := a.bulk(ctx); t.route == fleetapi.RouteTailnet {
		q.Set("gz", "1") // a slow link: gzip is cheaper than the bytes
	}
	resp, route, err := a.send(ctx, "GET", fleetapi.PathFilesTar+"?"+q.Encode(), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	size := int64(-1)
	if n, err := strconv.ParseInt(resp.Header.Get("X-Fleet-Bytes"), 10, 64); err == nil {
		size = n
	}
	r, err := hostfiles.Ungzip(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return &fleetapi.FileRead{Body: struct {
		io.Reader
		io.Closer
	}{r, resp.Body}, Size: size, Route: route}, nil
}

func (a *apiClient) FilesPut(ctx context.Context, dir, name, id string, off, size int64, body io.Reader) (*fleetapi.FilePut, error) {
	q := url.Values{"dir": {dir}, "name": {name}, "id": {id}, "offset": {strconv.FormatInt(off, 10)}, "size": {strconv.FormatInt(size, 10)}}
	if body == nil || size-off == 0 {
		body = http.NoBody
	}
	resp, _, err := a.send(ctx, "PUT", fleetapi.PathFilesPut+"?"+q.Encode(), body, size-off, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var p fleetapi.FilePut
	return &p, json.NewDecoder(resp.Body).Decode(&p)
}

func (a *apiClient) FilesHave(ctx context.Context, id string) (int64, error) {
	resp, _, err := a.send(ctx, "GET", fleetapi.PathFilesPut+"?id="+url.QueryEscape(id), nil, 0, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var p fleetapi.FilePut
	err = json.NewDecoder(resp.Body).Decode(&p)
	return p.Have, err
}

func (a *apiClient) FilesUntar(ctx context.Context, dir string, body io.Reader) (*fleetapi.FilePut, error) {
	if t := a.bulk(ctx); t.route == fleetapi.RouteTailnet { // gzip it on the way: a slow link
		pr, pw := io.Pipe()
		go func() {
			z, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
			_, err := io.Copy(z, body)
			if err == nil {
				err = z.Close()
			}
			pw.CloseWithError(err)
		}()
		defer pr.Close()
		body = pr
	}
	resp, _, err := a.send(ctx, "PUT", fleetapi.PathFilesUntar+"?dir="+url.QueryEscape(dir), body, -1, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var p fleetapi.FilePut
	return &p, json.NewDecoder(resp.Body).Decode(&p)
}
