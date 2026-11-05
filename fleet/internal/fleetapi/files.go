package fleetapi

import (
	"context"
	"io"
	"time"
)

// Files: fleetd's own machine's files, for the operator alone (admins), as
// the Files tab browses them. Paths are absolute there; ~ is the home of the
// user fleetd runs as.
const (
	// PathFiles: GET ?path=P is the directory at P (FileDir); a file's path
	// is its directory, with the file named in FileDir.Focus.
	PathFiles = "/v1/files"
	// PathFilesRead: GET ?path=P is the regular file's bytes, Range and all.
	PathFilesRead = "/v1/files/read"
	// PathFilesPreview: GET ?path=P&w=W&h=H is a picture of the image (or
	// video, or PDF page) at P, fit inside W×H pixels: a PNG or a JPEG.
	PathFilesPreview = "/v1/files/preview"
	// PathFilesTar: GET ?path=P[&gz=1] is a tar of the file or folder at P,
	// under its own name; X-Fleet-Bytes is what its files hold, when fleetd
	// counted them in time.
	PathFilesTar = "/v1/files/tar"
	// PathFilesPut: PUT ?dir=D&name=N&id=I&offset=O&size=S is bytes O.. of a
	// file of S bytes, upload I; the last of them puts it in D as N (or N 2,
	// when N is taken), and answers FilePut. GET ?id=I is how much of
	// upload I fleetd holds (FilePut.Have), to go on from there.
	PathFilesPut = "/v1/files/put"
	// PathFilesUntar: PUT ?dir=D is a tar (gzipped or not) of one folder,
	// unpacked into D under its own name (or "name 2").
	PathFilesUntar = "/v1/files/untar"
	// PathFilesOp: POST FileOp makes a folder, renames, or moves to the
	// trash (or deletes for good).
	PathFilesOp = "/v1/files/op"
	// PathFilesLAN: GET is how to reach the bulk routes (read, preview,
	// tar, put, untar) on fleetd's local network (FileLAN).
	PathFilesLAN = "/v1/files/lan"
)

// FilesMaxEntries is the most entries a directory answers with.
const FilesMaxEntries = 20_000

// File kinds.
const (
	KindDir   = "dir"
	KindFile  = "file"
	KindLink  = "link"  // a symlink; FileEntry.To is what it points at
	KindOther = "other" // a socket, a device, a pipe
)

// FileEntry is one entry of a directory.
type FileEntry struct {
	Name string    `json:"name"`
	Kind string    `json:"kind"`
	Size int64     `json:"size"`
	Mode string    `json:"mode"` // as ls shows it: -rw-r--r--
	Mod  time.Time `json:"mod"`
	// Link is a symlink's target as written; To is its kind (dir or file),
	// "" when it points at nothing.
	Link string `json:"link,omitempty"`
	To   string `json:"to,omitempty"`
}

// IsDir: a directory, or a link to one.
func (e FileEntry) IsDir() bool { return e.Kind == KindDir || e.Kind == KindLink && e.To == KindDir }

// FileDir is a directory and its entries, in no order.
type FileDir struct {
	Path    string      `json:"path"` // clean, absolute
	Home    string      `json:"home"`
	Host    string      `json:"host"`
	Entries []FileEntry `json:"entries"`
	// More is how many entries were left out past FilesMaxEntries.
	More int `json:"more,omitempty"`
	// Focus is the entry a file's path named.
	Focus string `json:"focus,omitempty"`
}

// FilePut is an upload as fleetd holds it.
type FilePut struct {
	Path string `json:"path,omitempty"` // where it went, once done
	Size int64  `json:"size"`
	Have int64  `json:"have"` // bytes of it fleetd holds
	Done bool   `json:"done"`
}

// File operations.
const (
	OpMkdir  = "mkdir"  // Path
	OpRename = "rename" // Path to To (a name, or a path)
	OpTrash  = "trash"  // Paths, to fleetd's trash, kept a week
	OpDelete = "delete" // Paths, for good
)

// FileOp is one operation on files.
type FileOp struct {
	Op    string   `json:"op"`
	Path  string   `json:"path,omitempty"`
	To    string   `json:"to,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// FileOpDone is an operation's result: the path made or renamed to, and
// for a trash, where each went.
type FileOpDone struct {
	Path  string   `json:"path,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// FileLAN is the files' bulk routes on fleetd's local network: HTTPS at any
// of Addrs, whose certificate's SHA-256 is Pin (hex), with Token as the
// bearer until Expires. A client that reaches one moves its files there,
// off the tailnet.
type FileLAN struct {
	Addrs   []string  `json:"addrs"`
	Pin     string    `json:"pin"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Routes a transfer takes.
const (
	RouteLAN     = "lan"     // fleetd's local network, directly
	RouteTailnet = "tailnet" // the API's own address
	RouteLocal   = "local"   // the same machine
)

// Files is fleetd's machine's files as a client reaches them.
type Files interface {
	FilesList(ctx context.Context, path string) (*FileDir, error)
	// FilesRead is the file's bytes from off, n of them (n < 0: to the
	// end), and its entry as read. ifMod, when set, is the time an earlier
	// read saw: a file changed since is read from 0, and FileRead.From says
	// so.
	FilesRead(ctx context.Context, path string, off, n int64, ifMod time.Time) (*FileRead, error)
	FilesPreview(ctx context.Context, path string, w, h int) ([]byte, error)
	FilesTar(ctx context.Context, path string) (*FileRead, error)
	FilesPut(ctx context.Context, dir, name, id string, off, size int64, body io.Reader) (*FilePut, error)
	// FilesHave is how much of upload id fleetd holds.
	FilesHave(ctx context.Context, id string) (int64, error)
	FilesUntar(ctx context.Context, dir string, body io.Reader) (*FilePut, error)
	FilesOp(ctx context.Context, op FileOp) (*FileOpDone, error)
	// FilesRoute is the route bulk calls take now.
	FilesRoute() string
}

// FileRead is a file's bytes on their way.
type FileRead struct {
	Body io.ReadCloser
	From int64     // the offset Body starts at
	Size int64     // the whole file's size; -1 unknown (a tar)
	Mod  time.Time // the file's time
	// Route is the route it came by.
	Route string
}
