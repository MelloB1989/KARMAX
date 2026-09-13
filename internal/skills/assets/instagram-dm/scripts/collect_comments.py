#!/usr/bin/env python3
"""Paginate a comment-list endpoint and write a deduped commenters.json.

Endpoint and headers are arguments, never constants — see the instagram-dm
SKILL.md, Step 2: they are discovered fresh from real browser traffic on
every run, because Instagram's internal endpoints are unversioned and move.

This script speaks only stdlib HTTP (urllib), so it has no dependency to
install before a run. It never prints the cookie file's contents, any
header value, or any raw Cookie header — only field NAMES and counts.

The response-shape field names below (--comments-field, --user-id-field,
etc.) have defaults, but they are guesses at a plausible shape, not
something observed from live Instagram traffic in this environment. Check
them against the response actually captured in SKILL.md Step 2 before
trusting a live run; override any of them that don't match.
"""

from __future__ import annotations

import argparse
import json
import os
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_USER_ID_FIELDS = ["user.pk", "user.id", "user_id", "owner.id", "owner.pk"]
DEFAULT_USERNAME_FIELDS = ["user.username", "owner.username", "username"]


def redact(err: BaseException) -> str:
    """A short, safe-to-print description of a request failure.

    Deliberately narrow: only the exception type and, for HTTP errors, the
    status code/reason. Never the exception's str() unverified — some
    libraries embed request headers in it, and this file's whole point is
    that a cookie value must never reach stdout/stderr.
    """
    if isinstance(err, urllib.error.HTTPError):
        return f"HTTP {err.code} {err.reason}"
    if isinstance(err, urllib.error.URLError):
        return f"connection error ({type(err.reason).__name__})"
    return type(err).__name__


def load_cookie_header(path: str) -> str:
    """Read the harvested session file and build a Cookie header.

    Every key/value in the file becomes a cookie pair — generic on purpose,
    so an operator can add an extra cookie without a script change. Only
    key NAMES are ever printed; values never are.
    """
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    try:
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)  # 0600, belt-and-suspenders
    except OSError:
        pass
    if not isinstance(data, dict) or not data:
        raise SystemExit(f"cookie file {path!r} did not contain a JSON object of cookies")
    print(f"loaded {len(data)} cookie(s) from {path}: {', '.join(sorted(data))}", file=sys.stderr)
    return "; ".join(f"{k}={v}" for k, v in data.items())


def load_headers(path: str | None) -> dict:
    if not path:
        return {}
    with open(path, "r", encoding="utf-8") as f:
        headers = json.load(f)
    if not isinstance(headers, dict):
        raise SystemExit(f"headers file {path!r} must be a JSON object")
    print(f"loaded {len(headers)} extra header(s) from {path}: {', '.join(sorted(headers))}", file=sys.stderr)
    return headers


def dotted_get(obj, path, default=None):
    cur = obj
    for part in path.split("."):
        if isinstance(cur, dict) and part in cur:
            cur = cur[part]
        else:
            return default
    return cur


def first_present(comment: dict, candidates: list[str]):
    for path in candidates:
        val = dotted_get(comment, path)
        if val not in (None, ""):
            return val
    return None


def add_query_param(url: str, param: str, value) -> str:
    parts = urllib.parse.urlsplit(url)
    query = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    query = [(k, v) for k, v in query if k != param]
    query.append((param, str(value)))
    return urllib.parse.urlunsplit(parts._replace(query=urllib.parse.urlencode(query)))


def fetch_json(url: str, cookie_header: str, extra_headers: dict, timeout: float) -> dict:
    req = urllib.request.Request(url, method="GET")
    req.add_header("Cookie", cookie_header)
    req.add_header("Accept", "application/json")
    for k, v in extra_headers.items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = resp.read()
    return json.loads(body)


def collect(args) -> list[dict]:
    cookie_header = load_cookie_header(args.cookie_file)
    extra_headers = load_headers(args.headers_file)
    user_id_fields = [args.user_id_field] + [
        f for f in DEFAULT_USER_ID_FIELDS if f != args.user_id_field
    ]
    username_fields = [args.username_field] + [
        f for f in DEFAULT_USERNAME_FIELDS if f != args.username_field
    ]

    commenters: dict[str, dict] = {}
    url = args.endpoint
    seen_urls: set[str] = set()

    for page_num in range(1, args.max_pages + 1):
        if url in seen_urls:
            print(f"page {page_num}: next URL repeats a page already fetched, stopping", file=sys.stderr)
            break
        seen_urls.add(url)

        try:
            page = fetch_json(url, cookie_header, extra_headers, args.timeout)
        except (urllib.error.HTTPError, urllib.error.URLError) as e:
            raise SystemExit(f"page {page_num}: request failed — {redact(e)}")

        comments = dotted_get(page, args.comments_field)
        if not isinstance(comments, list):
            raise SystemExit(
                f"page {page_num}: --comments-field {args.comments_field!r} did not resolve to a "
                "list in the response — check the shape captured in SKILL.md Step 2 and override "
                "--comments-field to match it"
            )

        new_this_page = 0
        for comment in comments:
            if not isinstance(comment, dict):
                continue
            uid = first_present(comment, user_id_fields)
            if uid is None:
                continue
            uid = str(uid)
            if uid not in commenters:
                username = first_present(comment, username_fields)
                commenters[uid] = {"id": uid, "username": username}
                new_this_page += 1

        print(f"page {page_num}: {len(comments)} comment(s), {new_this_page} new commenter(s), "
              f"{len(commenters)} unique so far", file=sys.stderr)

        if args.limit and len(commenters) >= args.limit:
            print(f"reached --limit {args.limit}, stopping pagination", file=sys.stderr)
            break

        next_url = dotted_get(page, args.next_url_field)
        if isinstance(next_url, str) and next_url:
            url = next_url
        else:
            has_more = dotted_get(page, args.has_more_field, False)
            cursor = dotted_get(page, args.cursor_field)
            if has_more and cursor is not None:
                url = add_query_param(args.endpoint, args.cursor_param, cursor)
            else:
                print(f"page {page_num}: no next page indicated, done", file=sys.stderr)
                break

        if page_num < args.max_pages:
            time.sleep(args.page_delay)
    else:
        print(f"hit --max-pages {args.max_pages} without the response saying pagination was done", file=sys.stderr)

    return sorted(commenters.values(), key=lambda c: c["id"])


def parse_args(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--endpoint", required=True, help="First-page comment-list URL, observed from real traffic (SKILL.md Step 2).")
    p.add_argument("--cookie-file", required=True, help="Path to the 0600 harvested-session JSON file.")
    p.add_argument("--headers-file", default=None, help="Path to a JSON object of extra headers observed from real traffic.")
    p.add_argument("--out", default="commenters.json", help="Where to write the deduped commenter list (default: commenters.json).")
    p.add_argument("--comments-field", default="comments", help="Dotted path to the list of comment objects in each page (default: comments).")
    p.add_argument("--user-id-field", default="user.pk", help="Dotted path to a commenter's id within a comment object (default: user.pk; other common shapes are tried as fallback).")
    p.add_argument("--username-field", default="user.username", help="Dotted path to a commenter's username (default: user.username).")
    p.add_argument("--next-url-field", default="next_url", help="Dotted path to a literal next-page URL, if the response provides one (default: next_url).")
    p.add_argument("--has-more-field", default="has_more", help="Dotted path to a has-more-pages boolean (default: has_more).")
    p.add_argument("--cursor-field", default="next_min_id", help="Dotted path to the next page's cursor value (default: next_min_id).")
    p.add_argument("--cursor-param", default="min_id", help="Query param name the cursor is sent back as (default: min_id).")
    p.add_argument("--max-pages", type=int, default=200, help="Safety bound on pagination (default: 200).")
    p.add_argument("--page-delay", type=float, default=0.5, help="Seconds to wait between pages (default: 0.5).")
    p.add_argument("--timeout", type=float, default=20.0, help="Per-request timeout in seconds (default: 20).")
    p.add_argument("--limit", type=int, default=None, help="Stop once this many unique commenters are collected (default: no limit).")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    commenters = collect(args)
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(commenters, f, indent=2)
        f.write("\n")
    print(f"wrote {len(commenters)} unique commenter(s) to {args.out}")


if __name__ == "__main__":
    main()
