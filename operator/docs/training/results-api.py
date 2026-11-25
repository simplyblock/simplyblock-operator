#!/usr/bin/env python3
# The results API of the operator training: the learners' accounts and their
# progress through the chapters (time spent and test attempts per chapter), and
# the anonymous statistics of the CRD quiz (crd-quiz.html).
#
#   GET  /api/results                       the public statistics: the quiz's
#                                           point totals, and per chapter the
#                                           points and pass of every attempt,
#                                           without names
#   POST /api/results                       one finished quiz, {"score": <int>}
#
#   POST /api/auth/register                 {"name", "email", "password"}
#   POST /api/auth/login                    {"email", "password"}
#   POST /api/auth/logout
#   GET  /api/me                            the signed-in learner's record
#   POST /api/me/password                   {"current", "password"}
#   POST /api/me/chapters/<id>/open         the chapter was opened
#   POST /api/me/chapters/<id>/time         {"seconds": <int>}, time spent
#   POST /api/me/chapters/<id>/attempts     one finished test,
#        {"points": <int>, "tasksRight": <int>, "tasksTotal": <int>, "passed": <bool>}
#
#   GET  /api/admin/learners                every learner's record   } with the
#   POST /api/admin/learners/<email>/reset  a temporary password     } header
#   POST /api/admin/learners/<email>/delete the account and record   } X-Admin-Token
#
# A learner registers with a name, an email address, and a password, and is
# signed in by a session cookie: HttpOnly, SameSite=Strict, and Secure when nginx
# reports the request came over HTTPS. Passwords are stored as salted PBKDF2
# hashes and session tokens as SHA-256 hashes, so the store holds neither in a
# usable form. ALLOWED_EMAIL_DOMAINS, a comma-separated list, restricts who may
# register; empty admits any address. The admin token is ADMIN_TOKEN; without it
# the admin endpoints answer 503. Both come from the environment training-host.sh
# writes. There is no email delivery, so a forgotten password is reset by an
# administrator, who hands the learner a temporary one.
#
# The store is one JSON file. The quiz keeps its anonymity, and the chapter
# statistics the pages show carry no name either: names and addresses are read
# by their owner and by the admin page only. The server keeps no request log.
import hashlib
import hmac
import json
import os
import re
import secrets
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PATH = os.environ.get("STATE_DIRECTORY", "/var/lib/crd-quiz") + "/results.json"
ADMIN_TOKEN = os.environ.get("ADMIN_TOKEN", "")
ALLOWED_DOMAINS = [d.strip().lower().lstrip("@") for d in os.environ.get("ALLOWED_EMAIL_DOMAINS", "").split(",") if d.strip()]
# A quiz or a chapter's questions score 2 * right - 75 over 75 judged answers,
# so a total is an odd integer between -75 and 75. Anything else did not come
# from the page.
MAX_POINTS = 75
CHAPTER_ID = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")
EMAIL = re.compile(r"^[^@\s]{1,64}@[a-z0-9.-]{1,190}\.[a-z]{2,24}$")
MAX_LEARNERS = 1000
MAX_ATTEMPTS = 200
MAX_SECONDS_PER_POST = 300
MIN_PASSWORD = 10
PBKDF2_ROUNDS = 240000
SESSION_DAYS = 30
COOKIE = "sb_training"
LOCK = threading.Lock()


def valid_points(value):
    return type(value) is int and -MAX_POINTS <= value <= MAX_POINTS and (value + MAX_POINTS) % 2 == 0


def valid_attempt(a):
    return (isinstance(a, dict) and valid_points(a.get("points")) and type(a.get("passed")) is bool
            and type(a.get("tasksRight")) is int and type(a.get("tasksTotal")) is int
            and 0 <= a.get("tasksRight") <= a.get("tasksTotal") <= 10 and type(a.get("at")) is int)


def hash_password(password, salt=None):
    salt = salt or secrets.token_bytes(16)
    digest = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, PBKDF2_ROUNDS)
    return salt.hex(), digest.hex()


def password_matches(password, account):
    try:
        _, digest = hash_password(password, bytes.fromhex(account["salt"]))
    except (KeyError, ValueError):
        return False
    return hmac.compare_digest(digest, account.get("hash", ""))


def token_hash(token):
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def empty_chapter():
    return {"seconds": 0, "openedAt": None, "lastAt": None, "attempts": []}


def load():
    try:
        with open(PATH, encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, ValueError):
        data = {}
    # The quiz alone wrote a bare list before the chapters existed, and the
    # name-only learners of the version before accounts are not carried over:
    # a record without a password is one nobody can sign in to.
    if isinstance(data, list):
        data = {"quiz": data}
    quiz = [s for s in data.get("quiz", []) if valid_points(s)]
    accounts = {}
    for email, acc in (data.get("accounts") or {}).items():
        if not isinstance(acc, dict) or not EMAIL.match(email) or not acc.get("hash"):
            continue
        chapters = {}
        for cid, ch in (acc.get("chapters") or {}).items():
            if CHAPTER_ID.match(cid) and isinstance(ch, dict):
                chapters[cid] = {
                    "seconds": ch.get("seconds") if type(ch.get("seconds")) is int and ch.get("seconds") >= 0 else 0,
                    "openedAt": ch.get("openedAt") if type(ch.get("openedAt")) is int else None,
                    "lastAt": ch.get("lastAt") if type(ch.get("lastAt")) is int else None,
                    "attempts": [a for a in ch.get("attempts", []) if valid_attempt(a)],
                }
        accounts[email] = {"name": str(acc.get("name") or email)[:60], "salt": acc["salt"], "hash": acc["hash"],
                           "mustChange": bool(acc.get("mustChange")),
                           "createdAt": acc.get("createdAt") if type(acc.get("createdAt")) is int else 0,
                           "chapters": chapters}
    now = int(time.time())
    sessions = {h: s for h, s in (data.get("sessions") or {}).items()
                if isinstance(s, dict) and s.get("email") in accounts and type(s.get("expires")) is int and s["expires"] > now}
    return {"quiz": quiz, "accounts": accounts, "sessions": sessions}


def save(data):
    tmp = PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f)
    os.replace(tmp, PATH)


def chapter_view(ch):
    # A chapter's status is derived: completed once any attempt passed,
    # in-progress once opened or attempted, open otherwise.
    attempts = ch["attempts"]
    passed = [a for a in attempts if a["passed"]]
    best = max(attempts, key=lambda a: (a["passed"], a["points"], a["tasksRight"])) if attempts else None
    status = "completed" if passed else "in-progress" if (ch["openedAt"] or attempts) else "open"
    return {"status": status, "seconds": ch["seconds"], "openedAt": ch["openedAt"], "lastAt": ch["lastAt"],
            "completedAt": min(a["at"] for a in passed) if passed else None,
            "attempts": attempts, "best": best}


def learner_view(email, acc):
    return {"email": email, "name": acc["name"], "createdAt": acc["createdAt"], "mustChange": acc["mustChange"],
            "chapters": {cid: chapter_view(ch) for cid, ch in acc["chapters"].items()}}


def public_summary(data):
    # The quiz page reads count, max, and scores at the top level; the chapter
    # statistics sit beside them, one entry per attempt and no name.
    chapters = {}
    for acc in data["accounts"].values():
        for cid, ch in acc["chapters"].items():
            chapters.setdefault(cid, []).extend({"points": a["points"], "passed": a["passed"]} for a in ch["attempts"])
    return {"count": len(data["quiz"]), "max": MAX_POINTS, "scores": data["quiz"],
            "chapters": {cid: {"count": len(e), "max": MAX_POINTS, "passed": sum(1 for x in e if x["passed"]), "scores": e}
                         for cid, e in chapters.items()}}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        # No request log: who did what is in the records alone.
        pass

    def reply(self, code, obj, cookie=None):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if cookie is not None:
            self.send_header("Set-Cookie", cookie)
        self.end_headers()
        self.wfile.write(body)

    def read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length > 1024:
            return None
        body = json.loads(self.rfile.read(length) or b"{}")
        if not isinstance(body, dict):
            raise ValueError("not an object")
        return body

    def secure(self):
        return self.headers.get("X-Forwarded-Proto", "") == "https"

    def session_cookie(self, token, max_age):
        attrs = ["%s=%s" % (COOKIE, token), "Path=/", "HttpOnly", "SameSite=Strict", "Max-Age=%d" % max_age]
        if self.secure():
            attrs.append("Secure")
        return "; ".join(attrs)

    def session_token(self):
        for part in (self.headers.get("Cookie") or "").split(";"):
            name, _, value = part.strip().partition("=")
            if name == COOKIE and value:
                return value
        return None

    def current(self, data):
        token = self.session_token()
        if not token:
            return None
        s = data["sessions"].get(token_hash(token))
        return s["email"] if s else None

    def new_session(self, data, email):
        token = secrets.token_urlsafe(32)
        data["sessions"][token_hash(token)] = {"email": email, "expires": int(time.time()) + SESSION_DAYS * 86400}
        return self.session_cookie(token, SESSION_DAYS * 86400)

    def admin_ok(self):
        if not ADMIN_TOKEN:
            self.reply(503, {"error": "no admin token is configured"})
            return False
        if not hmac.compare_digest(self.headers.get("X-Admin-Token", ""), ADMIN_TOKEN):
            self.reply(403, {"error": "wrong token"})
            return False
        return True

    # ---- routing --------------------------------------------------------
    def do_GET(self):
        if self.path == "/api/results":
            with LOCK:
                data = load()
            return self.reply(200, public_summary(data))
        if self.path == "/api/me":
            with LOCK:
                data = load()
                email = self.current(data)
            if not email:
                return self.reply(401, {"error": "not signed in"})
            return self.reply(200, learner_view(email, data["accounts"][email]))
        if self.path == "/api/admin/learners":
            if not self.admin_ok():
                return None
            with LOCK:
                data = load()
            return self.reply(200, {"learners": [learner_view(e, a) for e, a in sorted(data["accounts"].items(), key=lambda x: x[1]["name"].lower())]})
        return self.reply(404, {"error": "not found"})

    def do_POST(self):
        try:
            body = self.read_body()
        except (ValueError, TypeError):
            return self.reply(400, {"error": "expected a JSON object"})
        if body is None:
            return self.reply(413, {"error": "body too large"})
        if self.path == "/api/results":
            return self.post_quiz(body)
        if self.path == "/api/auth/register":
            return self.register(body)
        if self.path == "/api/auth/login":
            return self.login(body)
        if self.path == "/api/auth/logout":
            return self.logout()
        if self.path == "/api/me/password":
            return self.change_password(body)
        m = re.match(r"^/api/me/chapters/([^/]+)/(open|time|attempts)$", self.path)
        if m:
            return self.post_chapter(m.group(1), m.group(2), body)
        m = re.match(r"^/api/admin/learners/([^/]+)/(reset|delete)$", self.path)
        if m:
            return self.admin_action(m.group(1).replace("%40", "@").lower(), m.group(2))
        return self.reply(404, {"error": "not found"})

    # ---- the quiz -------------------------------------------------------
    def post_quiz(self, body):
        score = body.get("score")
        if not valid_points(score):
            return self.reply(400, {"error": "score must be an odd integer between -75 and 75"})
        with LOCK:
            data = load()
            data["quiz"].append(score)
            save(data)
        return self.reply(201, public_summary(data))

    # ---- accounts -------------------------------------------------------
    def register(self, body):
        name = " ".join(str(body.get("name") or "").split())
        email = str(body.get("email") or "").strip().lower()
        password = str(body.get("password") or "")
        if not name or len(name) > 60:
            return self.reply(400, {"error": "a name is 1 to 60 characters"})
        if not EMAIL.match(email):
            return self.reply(400, {"error": "that is not an email address"})
        if ALLOWED_DOMAINS and email.rsplit("@", 1)[1] not in ALLOWED_DOMAINS:
            return self.reply(400, {"error": "registration is open to addresses at " + ", ".join(ALLOWED_DOMAINS)})
        if len(password) < MIN_PASSWORD:
            return self.reply(400, {"error": "a password has at least %d characters" % MIN_PASSWORD})
        salt, digest = hash_password(password)
        with LOCK:
            data = load()
            if email in data["accounts"]:
                return self.reply(409, {"error": "an account with this address exists; sign in instead"})
            if len(data["accounts"]) >= MAX_LEARNERS:
                return self.reply(400, {"error": "too many accounts"})
            data["accounts"][email] = {"name": name, "salt": salt, "hash": digest, "mustChange": False,
                                       "createdAt": int(time.time()), "chapters": {}}
            cookie = self.new_session(data, email)
            save(data)
            view = learner_view(email, data["accounts"][email])
        return self.reply(201, view, cookie)

    def login(self, body):
        email = str(body.get("email") or "").strip().lower()
        password = str(body.get("password") or "")
        with LOCK:
            data = load()
            acc = data["accounts"].get(email)
        # The hash runs whether or not the account exists, so the answer takes
        # the same time either way and says nothing about which addresses exist.
        ok = password_matches(password, acc) if acc else (hash_password(password) and False)
        if not ok:
            return self.reply(401, {"error": "the address or the password is wrong"})
        with LOCK:
            data = load()
            if email not in data["accounts"]:
                return self.reply(401, {"error": "the address or the password is wrong"})
            cookie = self.new_session(data, email)
            save(data)
            view = learner_view(email, data["accounts"][email])
        return self.reply(200, view, cookie)

    def logout(self):
        token = self.session_token()
        if token:
            with LOCK:
                data = load()
                if data["sessions"].pop(token_hash(token), None):
                    save(data)
        return self.reply(200, {"ok": True}, self.session_cookie("", 0))

    def change_password(self, body):
        current, password = str(body.get("current") or ""), str(body.get("password") or "")
        if len(password) < MIN_PASSWORD:
            return self.reply(400, {"error": "a password has at least %d characters" % MIN_PASSWORD})
        with LOCK:
            data = load()
            email = self.current(data)
            if not email:
                return self.reply(401, {"error": "not signed in"})
            acc = data["accounts"][email]
        if not password_matches(current, acc):
            return self.reply(403, {"error": "the current password is wrong"})
        salt, digest = hash_password(password)
        with LOCK:
            data = load()
            acc = data["accounts"].get(email)
            if not acc:
                return self.reply(401, {"error": "not signed in"})
            acc.update({"salt": salt, "hash": digest, "mustChange": False})
            # Every other session of the account ends with the old password.
            mine = token_hash(self.session_token() or "")
            data["sessions"] = {h: s for h, s in data["sessions"].items() if s["email"] != email or h == mine}
            save(data)
            view = learner_view(email, acc)
        return self.reply(200, view)

    # ---- progress -------------------------------------------------------
    def post_chapter(self, cid, what, body):
        if not CHAPTER_ID.match(cid) or len(cid) > 64:
            return self.reply(404, {"error": "not found"})
        now = int(time.time())
        with LOCK:
            data = load()
            email = self.current(data)
            if not email:
                return self.reply(401, {"error": "not signed in"})
            ch = data["accounts"][email]["chapters"].setdefault(cid, empty_chapter())
            if what == "open":
                ch["openedAt"] = ch["openedAt"] or now
            elif what == "time":
                seconds = body.get("seconds")
                if type(seconds) is not int or seconds < 1 or seconds > MAX_SECONDS_PER_POST:
                    return self.reply(400, {"error": "seconds must be an integer between 1 and %d" % MAX_SECONDS_PER_POST})
                ch["seconds"] += seconds
                ch["openedAt"] = ch["openedAt"] or now
            else:
                attempt = {"points": body.get("points"), "tasksRight": body.get("tasksRight"),
                           "tasksTotal": body.get("tasksTotal"), "passed": body.get("passed"), "at": now}
                if not valid_attempt(attempt):
                    return self.reply(400, {"error": "expected points (odd integer between -75 and 75), tasksRight, tasksTotal, and passed"})
                if len(ch["attempts"]) >= MAX_ATTEMPTS:
                    return self.reply(400, {"error": "too many attempts"})
                ch["attempts"].append(attempt)
                ch["openedAt"] = ch["openedAt"] or now
            ch["lastAt"] = now
            save(data)
        return self.reply(201, chapter_view(ch))

    # ---- administration -------------------------------------------------
    def admin_action(self, email, action):
        if not self.admin_ok():
            return None
        with LOCK:
            data = load()
            acc = data["accounts"].get(email)
            if not acc:
                return self.reply(404, {"error": "no such account"})
            data["sessions"] = {h: s for h, s in data["sessions"].items() if s["email"] != email}
            if action == "delete":
                del data["accounts"][email]
                save(data)
                return self.reply(200, {"deleted": email})
            temporary = secrets.token_urlsafe(9)
            acc["salt"], acc["hash"] = hash_password(temporary)
            acc["mustChange"] = True
            save(data)
        return self.reply(200, {"email": email, "temporaryPassword": temporary})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
