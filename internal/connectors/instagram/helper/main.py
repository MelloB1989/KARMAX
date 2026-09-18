# The Instagram helper: instagrapi behind a line-oriented RPC on stdin/stdout.
#
# KARMAX is one Go binary and Instagram has no API for a personal account. The
# usable client for the private API is instagrapi, which is Python, so this runs
# as a child process rather than as a library — spawned on demand, killed with
# the connector, and speaking newline-delimited JSON over a pipe.
#
# stdio rather than a loopback port on purpose: there is no port to collide
# with, no token to leak, and the session cannot outlive the process holding
# the pipe.
#
# STDOUT IS THE PROTOCOL. Nothing may print there except one response object
# per line — a stray print() corrupts the stream and the Go side sees a parse
# error instead of a result. Diagnostics go to stderr, which KARMAX logs.

import datetime
import inspect
import json
import os
import stat
import sys
import traceback

# Everything the passthrough will call, named exactly.
#
# AN ALLOWLIST, NEVER A DENYLIST. instagrapi gains methods between releases, and
# a denylist hands each new one to the agent the day it lands — including the
# next way to write. Adding to this list is a deliberate act.
#
# Reads only. The singular/plural pairs are the trap worth knowing about:
# media_comments reads comments and media_comment POSTS one; media_likers reads
# who liked and media_like likes. A substring rule gets those backwards, which
# is why this is spelled out and a test asserts no known write method appears.
READS = frozenset(
    {
        "account_info",
        "media_pk_from_url",
        "media_pk_from_code",
        "media_code_from_pk",
        "media_id",
        "media_info",
        "media_comments",
        "media_likers",
        "media_user",
        "media_oembed",
        "user_info",
        "user_info_by_username",
        "user_id_from_username",
        "username_from_user_id",
        "user_followers",
        "user_following",
        "user_medias",
        "user_stories",
        "direct_threads",
        "direct_messages",
        "direct_thread",
        "direct_search",
        "direct_pending_inbox",
        "hashtag_info",
        "hashtag_medias_top",
        "hashtag_medias_recent",
        "location_info",
        "insights_media",
        "insights_account",
        "highlight_info",
        "story_info",
    }
)

# A reply has to fit through a pipe and then through an agent's context. A
# follower list runs to tens of thousands, so an unbounded read is a way to
# lose the conversation rather than a way to get an answer.
MAX_RESULT_BYTES = 256 * 1024

# Instagram's own anti-abuse signals. These are not retryable and they are not
# ordinary errors: continuing past one risks the operator's account, not just
# the call. The Go side refuses to retry anything flagged here.
HARD_STOP = {
    "FeedbackRequired",
    "ChallengeRequired",
    "CaptchaChallengeRequired",
    "ChallengeSelfieCaptcha",
    "ChallengeUnknownStep",
    "RecaptchaChallengeForm",
    "RateLimitError",
    "PleaseWaitFewMinutes",
    "ClientThrottledError",
    "SentryBlock",
    "LoginRequired",
    "ReloginAttemptExceeded",
    "ProxyAddressIsBlocked",
}


def log(msg):
    """Diagnostics, on stderr, where they cannot corrupt the protocol."""
    print(msg, file=sys.stderr, flush=True)


def to_jsonable(v):
    """Flatten instagrapi's pydantic models into something JSON can carry.

    model_dump(mode="json") rather than plain model_dump: the models hold
    datetimes and URL objects that the plain dump leaves as Python objects, and
    json.dumps then fails on a reply that looked fine right up to the moment it
    was sent."""
    if hasattr(v, "model_dump"):
        return v.model_dump(mode="json")
    if isinstance(v, (list, tuple, set)):
        return [to_jsonable(x) for x in v]
    if isinstance(v, dict):
        return {str(k): to_jsonable(x) for k, x in v.items()}
    if isinstance(v, (datetime.datetime, datetime.date)):
        return v.isoformat()
    if isinstance(v, (str, int, float, bool)) or v is None:
        return v
    return str(v)


class Helper:
    def __init__(self):
        self.client = None
        self.username = None
        self._warmed = False

    # ---- session -----------------------------------------------------------

    def _settings_path(self, username):
        """Where the device fingerprint lives, 0600 in a 0700 directory.

        Reused across runs deliberately: a new device every login is one of the
        strongest automation signals Instagram has."""
        home = os.path.expanduser("~")
        d = os.path.join(home, ".karmax", "instagram")
        os.makedirs(d, mode=0o700, exist_ok=True)
        safe = "".join(c for c in username.lower() if c.isalnum() or c in "._") or "account"
        return os.path.join(d, safe + ".settings.json")

    def _save_settings(self, path):
        self.client.dump_settings(path)
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)

    def _new_client(self):
        from instagrapi import Client

        cl = Client()
        # instagrapi's own pause between requests. Not the campaign-level
        # pacing a caller does on top; this is the floor.
        cl.delay_range = [5, 10]
        return cl

    # ---- methods -----------------------------------------------------------

    def ping(self, _params):
        """Liveness, and proof the venv actually has instagrapi in it.

        The version comes from package metadata because instagrapi exposes no
        __version__ attribute — asking for one gets you "unknown", which is
        exactly the wrong answer to have on hand when a run starts failing and
        the question is whether the library moved underneath it."""
        import importlib.metadata

        import instagrapi  # noqa: F401 — the import IS the check

        try:
            version = importlib.metadata.version("instagrapi")
        except importlib.metadata.PackageNotFoundError:
            version = "unknown"

        return {
            "ok": True,
            "instagrapi": version,
            "python": sys.version.split()[0],
            "logged_in": self.client is not None,
        }

    def login(self, params):
        """Sign in by session cookie, or by password.

        The session route is the one that matters: it reuses the login the
        operator already made in their own browser, so KARMAX asks for no
        password and opens no login flow. The password route stays because an
        unattended install has no browser to borrow from."""
        username = (params.get("username") or "").strip()
        sessionid = (params.get("sessionid") or "").strip()
        password = params.get("password") or ""
        totp_seed = (params.get("totp_seed") or "").strip()

        cl = self._new_client()

        # A stored fingerprint is only reusable when we know whose it is.
        settings_path = self._settings_path(username) if username else None
        if settings_path and os.path.exists(settings_path):
            try:
                cl.load_settings(settings_path)
            except Exception as e:
                # A corrupt settings file must not be fatal — it is a cache.
                log(f"settings unreadable, starting fresh: {type(e).__name__}")

        if sessionid:
            cl.login_by_sessionid(sessionid)
        elif username and password:
            # A seed is not a code. instagrapi wants the six digits, so the
            # seed is turned into them here — passing the seed straight through
            # fails with a message about the code being wrong, which sends
            # people looking at their authenticator app instead of at this.
            code = cl.totp_generate_code(totp_seed) if totp_seed else ""
            cl.login(username, password, verification_code=code)
        else:
            raise ValueError(
                "instagram: give either a sessionid, or a username and password"
            )

        self.client = cl
        info = cl.account_info()
        self.username = info.username

        # Settings are keyed by the account that actually came back, which is
        # not always the one asked for — a sessionid names its own account, and
        # writing it under a guessed username is how two accounts end up
        # sharing one fingerprint.
        self._save_settings(self._settings_path(info.username))

        return {"username": info.username, "pk": str(info.pk), "full_name": info.full_name}

    def account(self, _params):
        info = self._require().account_info()
        return {"username": info.username, "pk": str(info.pk), "full_name": info.full_name}

    def inbox(self, params):
        """Recent direct message threads. Read-only, as this connector has
        always been."""
        limit = params.get("limit") or 10
        try:
            limit = int(limit)
        except (TypeError, ValueError):
            limit = 10
        limit = max(1, min(limit, 30))

        threads = self._require().direct_threads(amount=limit)
        out = []
        for t in threads:
            last = ""
            if t.messages:
                last = t.messages[0].text or ""
            out.append(
                {
                    "thread_id": str(t.id),
                    "title": t.thread_title or "",
                    "last": last,
                    "last_active": t.last_activity_at.isoformat() if t.last_activity_at else "",
                    "users": [u.username for u in (t.users or [])],
                }
            )
        return {"count": len(out), "threads": out}

    def reads(self, _params):
        """What `call` will accept, with signatures.

        Discovery rather than documentation: this list is generated from the
        allowlist and the installed instagrapi, so it cannot drift from what the
        passthrough will actually do the way a hand-written list in a tool
        description would."""
        from instagrapi import Client

        out = []
        for name in sorted(READS):
            fn = getattr(Client, name, None)
            sig = str(inspect.signature(fn)).replace("self, ", "").replace("(self)", "()") if fn else ""
            doc = (inspect.getdoc(fn) or "").strip().split("\n")[0] if fn else ""
            out.append({"method": name, "signature": sig, "summary": doc[:120]})
        return {"count": len(out), "reads": out}

    def call(self, params):
        """Call one allowlisted instagrapi read.

        This exists so the agent is not limited to the handful of reads anyone
        thought to wrap. instagrapi has hundreds of methods and which one a task
        needs is not predictable — but which ones can damage the account is, and
        those are simply not reachable from here."""
        method = (params.get("method") or "").strip()
        args = params.get("args") or {}
        if not isinstance(args, dict):
            raise ValueError("instagram: 'args' must be an object of named arguments")

        if method not in READS:
            # Say what is available rather than only what is not: an agent that
            # guessed a plausible name can correct itself from this, and one
            # that wanted to write learns immediately that it cannot.
            near = sorted(m for m in READS if method and (method in m or m in method))
            hint = f" Did you mean: {', '.join(near[:5])}?" if near else ""
            raise PermissionError(
                f"instagram: {method!r} is not an allowed read. This passthrough is "
                f"read-only by design — sending, commenting, liking and following are "
                f"not reachable through it.{hint}"
            )

        # Checked against the class, not a signed-in client: a caller who
        # misspelled an argument should be told so without first being sent to
        # find credentials. Only the call itself needs a session.
        from instagrapi import Client

        unbound = getattr(Client, method)
        shown = inspect.Signature(list(inspect.signature(unbound).parameters.values())[1:])
        try:
            shown.bind(**args)
        except TypeError as e:
            # Name the real signature. "unexpected keyword argument" alone
            # leaves the agent guessing at what the right one was.
            raise TypeError(f"instagram: {method}{shown} — {e}") from None

        result = to_jsonable(getattr(self._require(), method)(**args))

        encoded = json.dumps(result)
        if len(encoded) > MAX_RESULT_BYTES:
            raise ValueError(
                f"instagram: {method} returned {len(encoded)} bytes, over the "
                f"{MAX_RESULT_BYTES} limit. Ask for less — most of these reads take an "
                f"'amount' argument."
            )
        return {"method": method, "result": result}

    def _warm(self):
        """Touch the messaging surface before writing to it.

        The one real run that got blocked did reads and four comment-writes
        happily, then took an immediate 403 on its first direct_send. Landing
        on the messaging endpoint cold, with a send, is the pattern that drew
        it. Once per process is enough; it is a signal, not a ritual."""
        if self._warmed:
            return
        try:
            self._require().direct_threads(amount=1)
        except Exception as e:  # noqa: BLE001 — warming is best-effort
            log(f"warm-up read failed, continuing: {type(e).__name__}")
        self._warmed = True

    def send_dm(self, params):
        """Send one direct message.

        One. The pacing, the cap and the ledger live in KARMAX, above this, so
        this deliberately has no loop in it — a helper that could send a batch
        would be a way to have those enforced on the batch rather than on each
        message."""
        text = (params.get("text") or "").strip()
        user_id = str(params.get("user_id") or "").strip()
        if not text:
            raise ValueError("instagram: a direct message needs text")
        if not user_id:
            raise ValueError("instagram: a direct message needs a user_id")
        self._warm()
        self._require().direct_send(text, user_ids=[int(user_id)])
        return {"sent": True, "user_id": user_id}

    def reply_comment(self, params):
        """Post one comment, optionally as a reply to another."""
        media_id = str(params.get("media_id") or "").strip()
        text = (params.get("text") or "").strip()
        replied_to = params.get("comment_id")
        if not media_id:
            raise ValueError("instagram: a comment needs a media_id")
        if not text:
            raise ValueError("instagram: a comment needs text")
        c = self._require().media_comment(
            media_id, text,
            replied_to_comment_id=int(replied_to) if replied_to else None,
        )
        return {"comment_pk": str(c.pk), "media_id": media_id}

    def _require(self):
        if self.client is None:
            raise RuntimeError("instagram: not signed in — call login first")
        return self.client


METHODS = {
    "ping": "ping",
    "login": "login",
    "account": "account",
    "inbox": "inbox",
    "call": "call",
    "reads": "reads",
    "send_dm": "send_dm",
    "reply_comment": "reply_comment",
}


def classify(exc):
    """Turn a Python exception into something the Go side can act on.

    The type name is preserved rather than folded into a taxonomy of our own:
    instagrapi's exception names are the vocabulary the operator will find when
    they search for what went wrong."""
    name = type(exc).__name__
    return {
        "type": name,
        "message": str(exc)[:500],
        "hard_stop": name in HARD_STOP,
    }


def main():
    helper = Helper()
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            # No id to answer with, so there is nobody to tell. Say it on
            # stderr and keep serving rather than dying on one bad line.
            log("dropped an unparseable request line")
            continue

        rid = req.get("id")
        method = req.get("method") or ""
        params = req.get("params") or {}

        attr = METHODS.get(method)
        if attr is None:
            resp = {"id": rid, "ok": False,
                    "error": {"type": "UnknownMethod",
                              "message": f"no method {method!r}", "hard_stop": False}}
        else:
            try:
                resp = {"id": rid, "ok": True, "result": getattr(helper, attr)(params)}
            except Exception as e:  # noqa: BLE001 — every failure is a reply
                log(f"{method} failed: {type(e).__name__}")
                log(traceback.format_exc(limit=3))
                resp = {"id": rid, "ok": False, "error": classify(e)}

        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
