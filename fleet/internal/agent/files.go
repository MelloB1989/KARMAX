package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// ErrPath is a path the container refused: not under /work or /tmp once its
// symlinks are followed, or not there.
var ErrPath = errors.New("path refused")

// The scripts take the two roots as $1 and $2 and the path as $3. The path
// is resolved here, in the container, with its symlinks: fleetd has only
// its name, and a link inside /work to somewhere else is no way out. /work
// is $FLEET_WORK where that is set, as everywhere else in the container.
const resolve = `w=$(readlink -m -- "${FLEET_WORK:-$1}") || exit 1
t=$(readlink -m -- "$2") || exit 1
p=$3; case "$p" in "$1"|"$1"/*) p=$w${p#"$1"} ;; esac
`

const putScript = resolve + `d=$(readlink -m -- "$p") || exit 1
case "$d" in "$w"|"$w"/*|"$t"|"$t"/*) ;; *) echo "$3 is $d: files go under $1 or $2 only" >&2; exit 3 ;; esac
mkdir -p -- "$d" && exec tar -x -p --no-same-owner -C "$d"
`

// getScript archives the path itself, under its own name; a symlink is
// archived as a link, not followed. A root is archived as its contents.
const getScript = resolve + `r=$(readlink -m -- "$p") || exit 1
case "$r" in "$w"|"$t") exec tar -c -C "$r" . ;; esac
b=$(basename -- "$p")
d=$(readlink -m -- "$(dirname -- "$p")") || exit 1
case "$d" in "$w"|"$w"/*|"$t"|"$t"/*) ;; *) echo "$3 is under $d: files come from under $1 or $2 only" >&2; exit 3 ;; esac
[ -e "$d/$b" ] || [ -L "$d/$b" ] || { echo "$3: no such file or directory" >&2; exit 4; }
exec tar -c -C "$d" -- "$b"
`

// PutFiles unpacks a tar stream into dir, which must be (after symlinks)
// under /work or /tmp. The stream is trusted to be filexfer.Sanitize's.
func (a *Agent) PutFiles(ctx context.Context, dir string, tarball io.Reader) error {
	res, err := a.C.Stream(ctx, tarball, io.Discard, "sh", "-c", putScript, "sh", "/work", "/tmp", dir)
	return filesErr("tar -x", res, err)
}

// GetFiles writes a tar of path, which must be under /work or /tmp, to w.
func (a *Agent) GetFiles(ctx context.Context, path string, w io.Writer) error {
	res, err := a.C.Stream(ctx, nil, w, "sh", "-c", getScript, "sh", "/work", "/tmp", path)
	return filesErr("tar -c", res, err)
}

// Rsh runs argv in the container, with stdin and stdout streamed; its exit
// status and the tail of its stderr are the result.
func (a *Agent) Rsh(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error) {
	return a.C.Stream(ctx, stdin, stdout, argv...)
}

func filesErr(what string, res target.Result, err error) error {
	switch {
	case err != nil:
		return err
	case res.Exit == 3 || res.Exit == 4:
		return fmt.Errorf("%w: %s", ErrPath, res.Stderr)
	case res.Exit != 0:
		return fmt.Errorf("%s in the container failed (exit %d): %s", what, res.Exit, res.Stderr)
	}
	return nil
}
