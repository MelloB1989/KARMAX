#!/usr/bin/env python3
"""normalize_id() — the one piece of logic in this skill that must never
guess, because guessing wrong here doesn't drop a DM or duplicate one, it
sends a DM to the WRONG PERSON.

Shared by collect_comments.py and send_dms.py (both auto-add their own
directory to sys.path, so `from ids import normalize_id` resolves here
regardless of the invoking shell's cwd) rather than copy-pasted, because
this got copy-pasted once already and the two copies would have drifted:
the correctness of every skip decision in send_dms.py depends on this
function agreeing with itself between collect_comments.py's dedupe and
send_dms.py's ledger reads.
"""

from __future__ import annotations

# Python floats are exact for every integer with |value| below this bound;
# at and above it, distinct integers start rounding to the same float, so
# "the float is integral" stops meaning "the float came from one exact id."
_MAX_EXACT_FLOAT_INT = 2**53


def normalize_id(value) -> str:
    """Canonicalize a recipient/ledger id to its exact digit string.

    Instagram user ids are 16-17 digits — past float's 53-bit exact-integer
    range — and Instagram's own APIs serialize them as JSON *strings* for
    exactly that reason. An earlier version of this function round-tripped
    every id through float(), including strings, "to normalize 1001 vs
    1001.0" — and that silently corrupted real ids: two different
    17-digit ids collapsed to the same string, and — worse — a real id
    got rewritten to a DIFFERENT real id, which would have put a
    stranger's account on the wire in an outgoing DM instead of the
    person who actually commented. That is not a bug this skill can
    afford to repeat, so the rule now is narrow and unforgiving:

    - `bool` -> rejected (it's an `int` subclass in Python and would
      normalize to "1"/"0").
    - `int` -> `str(x)`. Exact at any size — Python ints have no limit.
    - `float` -> accepted ONLY if integral AND within the exact-integer
      range above; anything else raises. An id is never fractional, so a
      non-integral float is corrupt input, not a real id that needs
      rounding — and a float outside the exact range cannot be trusted to
      mean the integer it looks like.
    - `str` -> `strip()`ed and used AS-IS. **Never parsed as a number.**
      The entire reason ids arrive as strings is that they are already
      exact; parsing one would reintroduce the exact bug above.
    - anything else (dict, list, None, ...) -> rejected.

    Raises ValueError, naming the bad value, on anything it won't
    normalize. Every caller must treat that as loud: exclude the one
    record it came from and say so on stderr, never swallow it — a
    dropped recipient nobody notices is how someone quietly never gets
    their link, and a *coerced* one is how a stranger gets a DM meant for
    someone else.
    """
    if isinstance(value, bool):
        raise ValueError(f"id {value!r} is a bool, not a real id")
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        if value.is_integer() and abs(value) < _MAX_EXACT_FLOAT_INT:
            return str(int(value))
        raise ValueError(f"id {value!r} is a non-integral or too-large float to safely normalize")
    if isinstance(value, str):
        s = value.strip()
        if not s:
            raise ValueError("id is an empty string")
        return s
    raise ValueError(f"id {value!r} (type {type(value).__name__}) is not a string, int, or float")
