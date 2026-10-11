package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// Killing the host-side `docker exec` does not kill what it started inside
// the container (observed on Docker 29): every console restart left its tail
// and tmux clients behind, and a closed control session left a read-write
// client. So every process the console starts in a container carries
// streamEnv in its environment, and
//   - a stream ends the in-container process itself: tail's wrapper exits
//     when its stdin closes, and a tmux client is detached by its marker;
//   - a console that starts sweeps what an earlier one left, marker only.
const streamEnv = "FLEET_CONSOLE_STREAM"

func newStreamID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// detachScript detaches the tmux clients whose process carries this stream's
// id ($1). The client's pid is the container's own, as tmux reports it, so
// /proc finds its environment.
const detachScript = `tmux list-clients -F '#{client_pid} #{client_tty}' 2>/dev/null | while read -r p t; do
	tr '\0' '\n' < "/proc/$p/environ" 2>/dev/null | grep -qx "` + streamEnv + `=$1" && tmux detach-client -t "$t"
done
exit 0`

// sweepScript ends every process left by an earlier console: anything whose
// environment has the marker. Nothing else is touched.
const sweepScript = `for d in /proc/[0-9]*; do
	p=${d#/proc/}
	[ "$p" = "$$" ] && continue
	tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -q '^` + streamEnv + `=' && kill "$p" 2>/dev/null
done
exit 0`

// withStreamEnv runs cmd inside the container with the marker set.
func withStreamEnv(id string, cmd ...string) []string {
	return append([]string{"env", streamEnv + "=" + id}, cmd...)
}

// detachClients detaches the tmux clients started under id. Best effort: the
// container may be gone, which detaches them anyway.
func detachClients(c target.Container, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Sh(ctx, detachScript, id)
}

// sweepLeftovers ends what an earlier console left in the container.
func sweepLeftovers(ctx context.Context, c target.Container) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.Sh(ctx, sweepScript)
	return err
}
