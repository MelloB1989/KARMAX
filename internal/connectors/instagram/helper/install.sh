#!/usr/bin/env bash
# Put the Instagram helper's Python environment on this machine, on demand.
#
# KARMAX ships as one static binary and this is the one feature that needs a
# second runtime, so the runtime is fetched when somebody first enables the
# connector rather than bundled into every install that will never use it.
#
# uv first, because it fetches its own interpreter: the machine's own python3
# may be too old, may have no pip, and on a Mac without the Xcode command line
# tools, touching /usr/bin/python3 triggers a multi-gigabyte install prompt.
# uv sidesteps all of that and never touches the system Python, so a failure
# here cannot break anything else on the machine.
#
# Idempotent: safe to run again on every start. It says what it did and exits
# non-zero with a reason if it could not.
set -euo pipefail

VENV="${KARMAX_INSTAGRAM_VENV:-$HOME/.karmax/instagram/venv}"
# A floor, not a pin. instagrapi tracks a target that moves without notice, so
# holding an install back to an exact version guarantees it eventually stops
# working; a floor keeps the known-good behaviour this was written against.
SPEC="${KARMAX_INSTAGRAPI_SPEC:-instagrapi>=3.0.2}"
PYVER="${KARMAX_INSTAGRAM_PYTHON:-3.12}"

say() { printf '%s\n' "$*" >&2; }

# Already working? Then there is nothing to do, and re-resolving the
# environment on every daemon start would just be slow and noisy.
if [ -x "$VENV/bin/python" ] && "$VENV/bin/python" -c "import instagrapi" 2>/dev/null; then
  say "instagram helper: already installed at $VENV"
  echo "$VENV/bin/python"
  exit 0
fi

find_uv() {
  if [ -n "${KARMAX_UV_PATH:-}" ] && [ -x "${KARMAX_UV_PATH}" ]; then
    echo "$KARMAX_UV_PATH"; return 0
  fi
  if command -v uv >/dev/null 2>&1; then command -v uv; return 0; fi
  for p in "$HOME/.local/bin/uv" "$HOME/.cargo/bin/uv" /opt/homebrew/bin/uv /usr/local/bin/uv; do
    [ -x "$p" ] && { echo "$p"; return 0; }
  done
  return 1
}

UV="$(find_uv || true)"

if [ -z "$UV" ]; then
  if [ "${KARMAX_INSTAGRAM_NO_FETCH:-}" = "1" ]; then
    say "instagram helper: uv is not installed and fetching is disabled"
    exit 3
  fi
  # This downloads and runs astral's installer. It is their documented install
  # path; KARMAX_UV_PATH exists so an operator who would rather vet and place
  # the binary themselves never has to take this route.
  say "instagram helper: installing uv"
  curl -LsSf https://astral.sh/uv/install.sh | sh >&2 || {
    say "instagram helper: could not install uv — install it yourself and set KARMAX_UV_PATH"
    exit 3
  }
  UV="$(find_uv || true)"
  [ -n "$UV" ] || { say "instagram helper: uv installed but not found on PATH"; exit 3; }
fi

say "instagram helper: creating $VENV"
mkdir -p "$(dirname "$VENV")"
chmod 700 "$(dirname "$VENV")" 2>/dev/null || true

# --python asks uv to supply the interpreter, downloading one if this machine
# has nothing suitable. That is the whole reason uv is preferred here.
"$UV" venv "$VENV" --python "$PYVER" >&2
"$UV" pip install --python "$VENV/bin/python" --quiet "$SPEC" >&2

# Prove it, rather than trusting that a silent install worked. A venv that
# exists but cannot import is the failure mode that otherwise shows up much
# later, as a confusing runtime error inside the connector.
"$VENV/bin/python" -c "import instagrapi" >&2 || {
  say "instagram helper: installed, but instagrapi will not import"
  exit 4
}

say "instagram helper: ready"
echo "$VENV/bin/python"
