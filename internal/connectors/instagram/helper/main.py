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

import json
import os
import stat
import sys
import traceback

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


class Helper:
    def __init__(self):
        self.client = None
        self.username = None

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

    def _require(self):
        if self.client is None:
            raise RuntimeError("instagram: not signed in — call login first")
        return self.client


METHODS = {
    "ping": "ping",
    "login": "login",
    "account": "account",
    "inbox": "inbox",
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
