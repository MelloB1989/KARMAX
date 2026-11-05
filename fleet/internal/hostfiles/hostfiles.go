// Package hostfiles is fleetd's own machine's files, for the operator: a
// directory's entries, a file's bytes, a picture of an image, a folder as a
// tar, uploads that go on where they broke off, and the few changes a file
// browser makes (a folder, a name, the trash). The Files tab of fleetctl's
// TUI is its user, through fleetd's API (fleetapi/files.go).
//
// fleetd runs as the operator's user, and an admin device may already run
// anything there through the desk: these routes give no more than that, and
// are the full scope's alone.
package hostfiles

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Errors, as the API answers them.
var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid")
	ErrExists    = errors.New("already there")
	ErrDenied    = errors.New("permission denied")
	ErrTooLarge  = errors.New("too large")
	ErrBusy      = errors.New("busy")
	ErrMismatch  = errors.New("not where the upload is")
	errOtherDisk = errors.New("on another disk")
)

func failf(kind error, format string, a ...any) error {
	return &kindErr{kind: kind, msg: fmt.Sprintf(format, a...)}
}

type kindErr struct {
	kind error
	msg  string
}

func (e *kindErr) Error() string        { return e.msg }
func (e *kindErr) Is(target error) bool { return target == e.kind }

// osErr is an error from the filesystem as the API says it: what is
// missing, what is not allowed.
func osErr(err error, p string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return failf(ErrNotFound, "%s: no such file or folder", p)
	case errors.Is(err, fs.ErrPermission):
		return failf(ErrDenied, "%s: permission denied", p)
	case errors.Is(err, fs.ErrExist):
		return failf(ErrExists, "%s is already there", p)
	case errors.Is(err, syscall.EXDEV):
		return failf(errOtherDisk, "%s is on another disk", p)
	}
	return err
}

// How long the trash and an unfinished upload are kept.
var (
	TrashKeep = 7 * 24 * time.Hour
	PartKeep  = 3 * 24 * time.Hour
)

// Manager is the files of the machine it runs on.
type Manager struct {
	// Home is ~; State is fleetd's state directory, where the trash and
	// the uploads under way are kept.
	Home, State string
	Host        string
	Now         func() time.Time
	Log         func(format string, a ...any)
	// Previews is how many pictures are made at once.
	Previews int

	once    sync.Once
	sem     chan struct{}
	mu      sync.Mutex
	putting map[string]bool
	swept   time.Time
}

// New is the files of this machine, fleetd's state in stateDir.
func New(stateDir string) *Manager {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	return &Manager{Home: home, State: stateDir, Host: host}
}

func (m *Manager) init() {
	m.once.Do(func() {
		if m.Now == nil {
			m.Now = time.Now
		}
		if m.Previews <= 0 {
			m.Previews = 2
		}
		m.sem = make(chan struct{}, m.Previews)
		m.putting = map[string]bool{}
	})
}

func (m *Manager) logf(format string, a ...any) {
	if m.Log != nil {
		m.Log("files: "+format, a...)
	}
}

func (m *Manager) trashDir() string { return filepath.Join(m.State, "trash") }
func (m *Manager) partDir() string  { return filepath.Join(m.State, "uploads") }

// Abs is p as a clean absolute path: ~ is Home, and a relative path is
// taken from Home.
func (m *Manager) Abs(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.ContainsRune(p, 0) {
		return "", failf(ErrInvalid, "%q: not a path", p)
	}
	switch {
	case p == "" || p == "~":
		p = m.Home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(m.Home, p[2:])
	case !filepath.IsAbs(p):
		p = filepath.Join(m.Home, p)
	}
	return filepath.Clean(p), nil
}

// List is the directory at p; a file's path is its directory, the file
// its focus.
func (m *Manager) List(p string) (fleetapi.FileDir, error) {
	m.init()
	abs, err := m.Abs(p)
	if err != nil {
		return fleetapi.FileDir{}, err
	}
	out := fleetapi.FileDir{Path: abs, Home: m.Home, Host: m.Host}
	fi, err := os.Stat(abs)
	if err != nil {
		return out, osErr(err, abs)
	}
	if !fi.IsDir() {
		out.Path, out.Focus = filepath.Dir(abs), filepath.Base(abs)
	}
	f, err := os.Open(out.Path)
	if err != nil {
		return out, osErr(err, out.Path)
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil && len(des) == 0 {
		return out, osErr(err, out.Path)
	}
	out.Entries = make([]fleetapi.FileEntry, 0, min(len(des), fleetapi.FilesMaxEntries))
	for _, de := range des {
		if len(out.Entries) == fleetapi.FilesMaxEntries {
			out.More++
			continue
		}
		out.Entries = append(out.Entries, entryOf(out.Path, de))
	}
	return out, nil
}

// entryOf is a directory entry as listed: a link with what it points at.
func entryOf(dir string, de fs.DirEntry) fleetapi.FileEntry {
	e := fleetapi.FileEntry{Name: de.Name(), Kind: kindOf(de.Type())}
	fi, err := de.Info()
	if err != nil {
		return e
	}
	e.Size, e.Mode, e.Mod = fi.Size(), fi.Mode().String(), fi.ModTime().UTC()
	if e.Kind == fleetapi.KindLink {
		e.Link, _ = os.Readlink(filepath.Join(dir, de.Name()))
		if to, err := os.Stat(filepath.Join(dir, de.Name())); err == nil {
			e.To = kindOf(to.Mode().Type())
			if e.To == fleetapi.KindFile {
				e.Size = to.Size()
			}
		}
	}
	if e.Kind == fleetapi.KindDir {
		e.Size = 0
	}
	return e
}

func kindOf(t fs.FileMode) string {
	switch {
	case t&fs.ModeSymlink != 0:
		return fleetapi.KindLink
	case t.IsDir():
		return fleetapi.KindDir
	case t.IsRegular():
		return fleetapi.KindFile
	}
	return fleetapi.KindOther
}

// Open is the regular file at p (through links), to read.
func (m *Manager) Open(p string) (*os.File, fs.FileInfo, error) {
	abs, err := m.Abs(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, osErr(err, abs)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, failf(ErrInvalid, "%s: not a file", abs)
	}
	return f, fi, nil
}

// unique is a path in dir for name that nothing has: name, then "name 2",
// "name 3", the extension kept.
func unique(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(name)
	if ext == name || len(ext) > 12 {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		p = filepath.Join(dir, stem+" "+strconv.Itoa(i)+ext)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
	}
}

// Unique is unique, for the client's own downloads.
func Unique(dir, name string) string { return unique(dir, name) }

// cleanName is a name for a file in a directory: no path in it.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 255 {
		return "", failf(ErrInvalid, "%q: not a file's name", name)
	}
	return name, nil
}

var partID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// Put writes bytes off.. of a file of size bytes, upload id, and once it
// has them all moves the file into dir as name (or "name 2"). An upload
// broken off goes on with Have.
func (m *Manager) Put(dir, name, id string, off, size int64, body io.Reader) (fleetapi.FilePut, error) {
	m.init()
	out := fleetapi.FilePut{Size: size}
	if !partID.MatchString(id) {
		return out, failf(ErrInvalid, "upload %q: not an upload's id", id)
	}
	name, err := cleanName(name)
	if err != nil {
		return out, err
	}
	if size < 0 || off < 0 || off > size {
		return out, failf(ErrInvalid, "bytes %d of %d: out of range", off, size)
	}
	d, err := m.Abs(dir)
	if err != nil {
		return out, err
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		if err != nil {
			return out, osErr(err, d)
		}
		return out, failf(ErrInvalid, "%s: not a folder", d)
	}
	m.mu.Lock()
	if m.putting[id] {
		m.mu.Unlock()
		return out, failf(ErrBusy, "upload %s is under way already", id)
	}
	m.putting[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.putting, id)
		m.mu.Unlock()
	}()
	if off == 0 {
		if free := freeSpace(d); free >= 0 && size > free-reserve {
			return out, failf(ErrTooLarge, "%s does not fit: %s free on fleetd's disk", bytesOf(size), bytesOf(free))
		}
	}
	if err := os.MkdirAll(m.partDir(), 0o700); err != nil {
		return out, err
	}
	part := filepath.Join(m.partDir(), id)
	var f *os.File
	if off == 0 {
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	} else {
		f, err = os.OpenFile(part, os.O_WRONLY, 0)
		if err == nil {
			if fi, serr := f.Stat(); serr != nil || fi.Size() != off {
				f.Close()
				out.Have = fileSize(part)
				return out, failf(ErrMismatch, "upload %s holds %d bytes, not %d", id, out.Have, off)
			}
			_, err = f.Seek(off, io.SeekStart)
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, failf(ErrMismatch, "upload %s: nothing of it is here", id)
		}
		return out, err
	}
	n, cerr := io.Copy(f, io.LimitReader(body, size-off))
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	out.Have = off + n
	if out.Have < size {
		if cerr == nil {
			cerr = failf(ErrInvalid, "upload %s: the body ended at %d of %d bytes", id, out.Have, size)
		}
		return out, cerr
	}
	dest := unique(d, name)
	if err := moveFile(part, dest); err != nil {
		return out, osErr(err, dest)
	}
	_ = os.Chmod(dest, 0o644)
	out.Path, out.Done = dest, true
	return out, nil
}

// Have is how much of upload id is here.
func (m *Manager) Have(id string) (int64, error) {
	if !partID.MatchString(id) {
		return 0, failf(ErrInvalid, "upload %q: not an upload's id", id)
	}
	return fileSize(filepath.Join(m.partDir(), id)), nil
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// moveFile renames, or across disks copies and removes.
func moveFile(from, to string) error {
	err := os.Rename(from, to)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(to)
		return err
	}
	return os.Remove(from)
}

// reserve is the space an upload leaves free on fleetd's disk.
const reserve = 1 << 30

func bytesOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Op makes a folder, renames, trashes or deletes.
func (m *Manager) Op(op fleetapi.FileOp) (fleetapi.FileOpDone, error) {
	m.init()
	var out fleetapi.FileOpDone
	switch op.Op {
	case fleetapi.OpMkdir:
		p, err := m.Abs(op.Path)
		if err != nil {
			return out, err
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return out, osErr(err, p)
		}
		out.Path = p
	case fleetapi.OpRename:
		from, err := m.Abs(op.Path)
		if err != nil {
			return out, err
		}
		to := strings.TrimSpace(op.To)
		if !strings.Contains(to, "/") && to != "~" {
			if to, err = cleanName(to); err != nil {
				return out, err
			}
			to = filepath.Join(filepath.Dir(from), to)
		} else if to, err = m.Abs(to); err != nil {
			return out, err
		}
		if err := m.guard(from); err != nil {
			return out, err
		}
		if _, err := os.Lstat(to); err == nil {
			return out, failf(ErrExists, "%s is already there", to)
		}
		if err := os.Rename(from, to); err != nil {
			return out, osErr(err, from)
		}
		out.Path = to
	case fleetapi.OpTrash, fleetapi.OpDelete:
		if len(op.Paths) == 0 && op.Path != "" {
			op.Paths = []string{op.Path}
		}
		if len(op.Paths) == 0 {
			return out, failf(ErrInvalid, "%s: no paths", op.Op)
		}
		for _, raw := range op.Paths {
			p, err := m.Abs(raw)
			if err != nil {
				return out, err
			}
			if err := m.guard(p); err != nil {
				return out, err
			}
			if _, err := os.Lstat(p); err != nil {
				return out, osErr(err, p)
			}
			if op.Op == fleetapi.OpDelete {
				if err := os.RemoveAll(p); err != nil {
					return out, osErr(err, p)
				}
				out.Paths = append(out.Paths, p)
				continue
			}
			to, err := m.trash(p)
			if errors.Is(err, errOtherDisk) {
				return out, failf(ErrInvalid, "%s is on another disk than fleetd's trash: delete it for good instead", p)
			}
			if err != nil {
				return out, err
			}
			out.Paths = append(out.Paths, to)
		}
	default:
		return out, failf(ErrInvalid, "op %q: want mkdir, rename, trash or delete", op.Op)
	}
	return out, nil
}

// guard refuses to move or remove what holds the operator's machine
// together: the root, home, any folder above home, and fleetd's own state.
func (m *Manager) guard(p string) error {
	switch {
	case p == "/" || p == filepath.Dir(p):
		return failf(ErrInvalid, "%s: not that", p)
	case p == m.Home || strings.HasPrefix(m.Home+"/", p+"/"):
		return failf(ErrInvalid, "%s holds the home folder: not that", p)
	case m.State != "" && (p == m.State || strings.HasPrefix(m.State+"/", p+"/")):
		return failf(ErrInvalid, "%s holds fleetd's state: not that", p)
	}
	return nil
}

// trash moves p into the trash: <state>/trash/<day>/<random>/<name>.
func (m *Manager) trash(p string) (string, error) {
	dir := filepath.Join(m.trashDir(), m.Now().Format("2006-01-02"), randID(6))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	to := filepath.Join(dir, filepath.Base(p))
	if err := os.Rename(p, to); err != nil {
		os.Remove(dir)
		return "", osErr(err, p)
	}
	return to, nil
}

// Sweep empties the trash of what is older than TrashKeep, and the
// uploads left unfinished for PartKeep; at most once an hour.
func (m *Manager) Sweep() {
	m.init()
	now := m.Now()
	m.mu.Lock()
	if now.Sub(m.swept) < time.Hour {
		m.mu.Unlock()
		return
	}
	m.swept = now
	m.mu.Unlock()
	days, _ := os.ReadDir(m.trashDir())
	for _, d := range days {
		t, err := time.ParseInLocation("2006-01-02", d.Name(), now.Location())
		if err != nil || !d.IsDir() {
			continue
		}
		if now.Sub(t.AddDate(0, 0, 1)) > TrashKeep {
			if err := os.RemoveAll(filepath.Join(m.trashDir(), d.Name())); err != nil {
				m.logf("sweep the trash: %v", err)
			}
		}
	}
	parts, _ := os.ReadDir(m.partDir())
	for _, p := range parts {
		if fi, err := p.Info(); err == nil && now.Sub(fi.ModTime()) > PartKeep {
			os.Remove(filepath.Join(m.partDir(), p.Name()))
		}
	}
}

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUploadID is an id for an upload.
func NewUploadID() string { return randID(12) }
