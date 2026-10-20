package target

import (
	"os/exec"
	"time"
)

// WaitDelay bounds how long Wait lingers for a killed command's pipes to
// close. `docker --context pc2 …` runs ssh as a child, and ssh can keep the
// stdout and stderr pipes open after docker is killed; without a bound,
// cmd.Wait blocks on the copy goroutines for as long as ssh lives.
const WaitDelay = 5 * time.Second

// HardenCtx is Harden for a command made by exec.CommandContext: when its
// context ends, the whole group is killed, and Wait gives up on pipes that
// grandchildren hold after WaitDelay.
func HardenCtx(cmd *exec.Cmd) *exec.Cmd {
	Harden(cmd)
	cmd.Cancel = func() error { return KillGroup(cmd) }
	return cmd
}
