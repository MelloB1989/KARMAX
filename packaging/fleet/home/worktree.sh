#!/bin/sh
# worktree.sh — the fleet's side of worktrunk (wt) in an agent's container.
#
#   worktree.sh mine <path>  exit 0 when <path> is this agent's worktree: under
#                            /work/wt (its slots share it), or under
#                            <code>/.worktrees/<agent>/ (or a slot's <agent>-<k>
#                            or <agent>:<k>). wt's pre-remove hook, from
#                            /etc/xdg/worktrunk/config.toml.
#   worktree.sh create       Claude Code's WorktreeCreate hook: {"name": …} on
#                            stdin, the new worktree's path on stdout. The
#                            branch is fleet/<agent>/<name>.
#   worktree.sh remove       Claude Code's WorktreeRemove hook:
#                            {"worktree_path": …} on stdin.
set -u
work=${FLEET_WORK:-/work}
code=${FLEET_CODE:-$HOME/code}
agent=${FLEET_AGENT:-}
agent=${agent%%:*}

mine() {
	case "$1" in
	"$work"/wt/*) return 0 ;;
	esac
	[ -n "$agent" ] || return 1
	case "$1" in
	"$code/.worktrees/$agent"/* | "$code/.worktrees/$agent"-*/* | "$code/.worktrees/$agent":*/*) return 0 ;;
	esac
	return 1
}

case "${1:-}" in
mine)
	mine "${2:-}" && exit 0
	echo "fleet: ${2:-?} is not one of ${agent:-this agent}'s worktrees; leave it to whoever made it" >&2
	exit 1
	;;
create)
	in=$(cat)
	name=$(printf '%s' "$in" | jq -er .name) || exit 1
	# The repository the session is in: where it cd'd to, else its project.
	repo=$(printf '%s' "$in" | jq -r '.cwd // empty')
	git -C "${repo:-.}" rev-parse --git-dir >/dev/null 2>&1 || repo=${CLAUDE_PROJECT_DIR:-$PWD}
	if ! git -C "$repo" rev-parse --git-dir >/dev/null 2>&1; then
		echo "fleet: $repo is not a git repository; cd into one under $code first" >&2
		exit 1
	fi
	case "$name" in
	*/*) branch=$name ;;
	*) branch="fleet/$agent/$name" ;;
	esac
	create=--create
	git -C "$repo" rev-parse --verify --quiet "refs/heads/$branch" >/dev/null 2>&1 && create=
	wt -C "$repo" switch $create "$branch" --no-cd --format=json | jq -er .path
	;;
remove)
	p=$(jq -er .worktree_path) || exit 1
	[ -e "$p/.git" ] || exit 0
	mine "$p" || {
		echo "fleet: $p is not one of ${agent:-this agent}'s worktrees" >&2
		exit 1
	}
	# Locked when made (worktrunk.toml), and wt leaves a locked one alone; if
	# it is not removed after all, it is locked again.
	git -C "$p" worktree unlock "$p" 2>/dev/null
	wt -C "$p" remove --foreground "$p" || {
		git -C "$p" worktree lock --reason fleet "$p" 2>/dev/null
		exit 1
	}
	;;
*)
	sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
