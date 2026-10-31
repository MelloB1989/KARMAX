// Package cleanup deletes what finished work leaves behind: an agent's task
// checkouts and scratch under /work, the caches it filled, and its checkouts
// under the shared code's .worktrees. One shell script does it, in an
// agent's container or on fleetd's own machine, and says what it did.
//
// An item goes once nothing in it was written, changed or read since a
// cutoff, and only if no git checkout in it holds work found nowhere else:
// changes not committed, or commits no remote has. Those are kept and said.
package cleanup

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Script cleans up. $1 is the cutoff (unix seconds), $2 is 1 for a dry run,
// $3 is the mode: work (an agent's container: its /work and its caches) or
// dirs (every entry of the directories that follow). It prints one line per
// item it did not leave alone: <deleted|would|kept> TAB <KiB> TAB <path> TAB
// <why>. An item in use is not one yet, and is not said.
//
// In /work, .fleet (fleetd's and agent-run's own files), .claude (Claude
// Code's, for sessions in /work) and code (a sandbox's code volume) are never
// items, nor is any mount; wt, inbox and .secrets are directories of items. Caches are not anyone's work, so no git check: Go's
// build cache goes by entry (Go marks an entry used by its mtime), Go's
// module cache by module version and download zip, and every other cache
// directory by what sits in it.
const Script = `cut=$1 dry=$2 mode=$3
shift 3
ref=$(mktemp) || exit 1
trap 'rm -f "$ref"' EXIT
touch -d "@$cut" "$ref" || exit 1
say() { printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4"; }
kib() { du -skx -- "$1" 2>/dev/null | cut -f1; }
plural() { if [ "$1" = 1 ]; then echo "1 $2"; else echo "$1 $3"; fi; }

# used P: anything in P written, changed or read since the cutoff, or a part
# of it that could not be looked at. Reading a directory is not using it
# (this very walk reads them), nor is git reading its own files (unsafe runs
# git, a dry run too): only other files' reads count.
used() {
	u=$(find "$1" -xdev \( -newer "$ref" -o -cnewer "$ref" -o \( -type f -anewer "$ref" ! -name .git ! -path '*/.git/*' \) \) -print -quit 2>/dev/null)
	rc=$?
	[ -n "$u" ] || [ "$rc" -ne 0 ]
}

# unsafe P says why deleting P would lose work found nowhere else: a git
# checkout in it with changes not committed, or with commits no remote has
# (a worktree's own HEAD; a repository of its own, every branch too).
unsafe() {
	find "$1" -xdev -name .git -prune -print 2>/dev/null | while IFS= read -r g; do
		d=${g%/.git}
		if ! s=$(git -C "$d" --no-optional-locks status --porcelain 2>/dev/null); then
			echo "git cannot read $d"; break
		fi
		if [ -n "$s" ]; then echo "uncommitted changes in $d"; break; fi
		refs=
		if git -C "$d" rev-parse -q --verify HEAD >/dev/null 2>&1; then refs=HEAD; fi
		if [ -d "$g" ]; then refs="$refs --branches"; fi
		[ -n "$refs" ] || continue
		if ! n=$(git -C "$d" rev-list --count $refs --not --remotes 2>/dev/null); then
			echo "git cannot count the commits in $d"; break
		fi
		if [ "$n" != 0 ]; then echo "$(plural "$n" commit commits) no remote has in $d"; break; fi
	done
}

# drop P deletes it, and says whether it is gone. A git worktree in it goes
# through git first, so its repository forgets it, lock and all.
drop() {
	if [ -L "$1" ]; then rm -f -- "$1"; return; fi
	repos=$(find "$1" -xdev -name .git -type f -prune -print 2>/dev/null | while IFS= read -r g; do
		d=${g%/.git}
		gd=$(git -C "$d" rev-parse --path-format=absolute --git-dir 2>/dev/null) || continue
		common=$(git -C "$d" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || continue
		case $gd in "$common"/worktrees/*) ;; *) continue ;; esac # a submodule's .git file: not a worktree
		if ! git --git-dir="$common" worktree remove -f -f "$d" >/dev/null 2>&1; then
			git --git-dir="$common" worktree unlock "$d" >/dev/null 2>&1
			printf '%s\n' "$common"
		fi
	done)
	chmod -R u+w -- "$1" 2>/dev/null # Go's module cache is read-only
	rm -rf --one-file-system -- "$1" 2>/dev/null
	printf '%s\n' "$repos" | while IFS= read -r r; do
		[ -z "$r" ] || git --git-dir="$r" worktree prune >/dev/null 2>&1
	done
	[ ! -e "$1" ]
}

# item P: an entry of finished work.
item() {
	used "$1" && return 0
	k=$(kib "$1")
	why=$(unsafe "$1")
	if [ -n "$why" ]; then say kept "$k" "$1" "$why"
	elif [ "$dry" = 1 ]; then say would "$k" "$1" unused
	elif drop "$1"; then say deleted "$k" "$1" unused
	else say kept "$k" "$1" "could not delete all of it"
	fi
}

# cached P: a cache's entry.
cached() {
	used "$1" && return 0
	k=$(kib "$1")
	if [ "$dry" = 1 ]; then say would "$k" "$1" "cache, unused"
	elif drop "$1"; then say deleted "$k" "$1" "cache, unused"
	else say kept "$k" "$1" "could not delete all of it"
	fi
}

# sweep C: the cache entries of C on stdin, one line for all that go.
sweep() {
	n=0 k=0
	while IFS= read -r e; do
		used "$e" && continue
		s=$(kib "$e")
		if [ "$dry" != 1 ] && ! drop "$e"; then say kept "$s" "$e" "could not delete all of it"; continue; fi
		n=$((n + 1)) k=$((k + ${s:-0}))
	done
	[ "$n" != 0 ] || return 0
	if [ "$dry" = 1 ]; then a=would; else a=deleted; fi
	say "$a" "$k" "$1" "$(plural "$n" "cache entry" "cache entries") unused"
}

entries() { for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do if [ -e "$e" ] || [ -L "$e" ]; then printf '%s\n' "$e"; fi; done; }

case $mode in
work)
	W=${FLEET_WORK:-/work}
	dev=$(stat -c %d "$W") || exit 1
	entries "$W" | while IFS= read -r e; do
		case ${e##*/} in .fleet | .claude | code | wt | inbox | .secrets) continue ;; esac
		[ -L "$e" ] || [ "$(stat -c %d "$e")" = "$dev" ] || continue # a mount
		item "$e"
	done
	for d in wt inbox .secrets; do
		entries "$W/$d" | while IFS= read -r e; do item "$e"; done
	done
	H=$HOME
	gc=${GOCACHE:-$H/.cache/go-build}
	gp=${GOPATH:-$H/go}
	gm=${GOMODCACHE:-${gp%%:*}/pkg/mod}
	if [ -d "$gc" ]; then
		if [ "$dry" = 1 ]; then a=would del=; else a=deleted del=-delete; fi
		set -- $(find "$gc" -xdev -type f ! -newer "$ref" -printf '%k\n' $del 2>/dev/null | awk '{n++; k+=$1} END {print n+0, k+0}')
		[ "${1:-0}" = 0 ] || say "$a" "$2" "$gc" "$(plural "$1" "cache entry" "cache entries") unused"
	fi
	if [ -d "$gm" ]; then
		find "$gm" -xdev -path "$gm/cache" -prune -o -type d -name '*@*' -prune -print 2>/dev/null | sweep "$gm"
		if [ -d "$gm/cache/download" ]; then
			if [ "$dry" = 1 ]; then a=would del=; else a=deleted del=-delete; fi
			set -- $(find "$gm/cache/download" -xdev -type f -name '*.zip' ! -newer "$ref" ! -anewer "$ref" -printf '%k\n' $del 2>/dev/null | awk '{n++; k+=$1} END {print n+0, k+0}')
			[ "${1:-0}" = 0 ] || say "$a" "$2" "$gm/cache/download" "$(plural "$1" "module download" "module downloads") unused"
		fi
	fi
	for c in "$H/.cache/ms-playwright" "$H/.cache" "$H/.npm" "$H/.local/share/pnpm"; do
		entries "$c" | while IFS= read -r e; do
			case $e in "$gc" | "$H/.cache/ms-playwright") continue ;; esac
			cached "$e"
		done
	done
	;;
dirs)
	for d; do
		entries "$d" | while IFS= read -r e; do item "$e"; done
	done
	;;
*)
	echo "cleanup: no mode $mode" >&2
	exit 2
	;;
esac
exit 0
`

// NotDeleted is why an item the script tried to delete is kept: something
// in it would not go (a mount, a file it may not remove).
const NotDeleted = "could not delete all of it"

// Work is the script's arguments for an agent's container.
func Work(cut time.Time, dry bool) []string { return args(cut, dry, "work") }

// Dirs is the script's arguments for the entries of dirs, on fleetd's own
// machine.
func Dirs(cut time.Time, dry bool, dirs ...string) []string {
	return append(args(cut, dry, "dirs"), dirs...)
}

func args(cut time.Time, dry bool, mode string) []string {
	d := "0"
	if dry {
		d = "1"
	}
	return []string{strconv.FormatInt(cut.Unix(), 10), d, mode}
}

// Local runs the script on this machine.
func Local(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", Script, "sh"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Parse reads the script's lines. Where is left for the caller to say.
func Parse(out []byte) ([]fleetapi.CleanupItem, error) {
	var items []fleetapi.CleanupItem
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" {
			continue
		}
		f := strings.SplitN(l, "\t", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("cleanup line %q", l)
		}
		it := fleetapi.CleanupItem{Path: f[2], Why: f[3]}
		switch f[0] {
		case "deleted", "kept":
			it.Action = f[0]
		case "would":
			it.Action = fleetapi.CleanupWould
		default:
			return nil, fmt.Errorf("cleanup line %q: no action %q", l, f[0])
		}
		if f[1] != "" {
			k, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("cleanup line %q: %w", l, err)
			}
			it.Bytes = k << 10
		}
		items = append(items, it)
	}
	return items, nil
}
