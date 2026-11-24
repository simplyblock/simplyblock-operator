#!/usr/bin/env python3
# The results API of the operator training: the learners' progress through the
# chapters, the attempts of every chapter test, the time spent per chapter, and
# the anonymous statistics of the CRD quiz (crd-quiz.html).
#
#   GET  /api/results                         the public statistics: the quiz's
#                                             point totals, and per chapter the
#                                             points and pass of every attempt,
#                                             without names
#   POST /api/results                         one finished quiz, {"score": <int>}
#   POST /api/learners                        sign in, {"name": "<name>"}; creates
#                                             the learner when the name is new
#   GET  /api/learners/<key>                  one learner's record
#   POST /api/learners/<key>/chapters/<id>/open      the chapter was opened
#   POST /api/learners/<key>/chapters/<id>/time      {"seconds": <int>}, time spent
#   POST /api/learners/<key>/chapters/<id>/attempts  one finished test,
#        {"points": <int>, "tasksRight": <int>, "tasksTotal": <int>, "passed": <bool>}
#   GET  /api/admin/learners                  every learner's record, with the
#                                             header X-Admin-Token
#
# A learner is the name they type, with no password: the training is internal,
# and a name is what the people reading the admin page know each other by. The
# admin token is the ADMIN_TOKEN environment variable, set by training-host.sh;
# without it the admin endpoint answers 503. The store is one JSON file. The
# quiz keeps its anonymity, and the chapter statistics the pages show carry no
# name either; the names live in the learners' records, which only their owner
# and the admin page read. The server keeps no request log.
import json
import os
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PATH = os.environ.get("STATE_DIRECTORY", "/var/lib/crd-quiz") + "/results.json"
ADMIN_TOKEN = os.environ.get("ADMIN_TOKEN", "")
# A quiz or a chapter's questions score 2 * right - 75 over 75 judged answers,
# so a total is an odd integer between -75 and 75. Anything else did not come
# from the page.
MAX_POINTS = 75
CHAPTER_ID = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")
MAX_LEARNERS = 500
MAX_ATTEMPTS = 200
MAX_SECONDS_PER_POST = 300
LOCK = threading.Lock()


def valid_points(value):
    return type(value) is int and -MAX_POINTS <= value <= MAX_POINTS and (value + MAX_POINTS) % 2 == 0


def learner_key(name):
    # The key a name is stored under: lowercase, single spaces, letters, digits,
    # and a few marks. Two spellings of one name that differ in case or spacing
    # are one learner.
    key = re.sub(r"[^a-z0-9 .'-]", "", " ".join(name.lower().split()))
    return key.strip()


def valid_attempt(a):
    return (isinstance(a, dict) and valid_points(a.get("points")) and type(a.get("passed")) is bool
            and type(a.get("tasksRight")) is int and type(a.get("tasksTotal")) is int
            and 0 <= a.get("tasksRight") <= a.get("tasksTotal") <= 10 and type(a.get("at")) is int)


def load():
    try:
        with open(PATH, encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, ValueError):
        return {"quiz": [], "learners": {}}
    # The quiz alone wrote a bare list before the chapters existed.
    if isinstance(data, list):
        data = {"quiz": data, "learners": {}}
    quiz = [s for s in data.get("quiz", []) if valid_points(s)]
    learners = {}
    for key, rec in data.get("learners", {}).items():
        if not isinstance(rec, dict) or learner_key(key) != key or not key:
            continue
        chapters = {}
        for cid, ch in rec.get("chapters", {}).items():
            if CHAPTER_ID.match(cid) and isinstance(ch, dict):
                chapters[cid] = {
                    "seconds": ch.get("seconds") if type(ch.get("seconds")) is int and ch.get("seconds") >= 0 else 0,
                    "openedAt": ch.get("openedAt") if type(ch.get("openedAt")) is int else None,
                    "lastAt": ch.get("lastAt") if type(ch.get("lastAt")) is int else None,
                    "attempts": [a for a in ch.get("attempts", []) if valid_attempt(a)],
                }
        learners[key] = {"name": str(rec.get("name") or key)[:60], "createdAt": rec.get("createdAt") if type(rec.get("createdAt")) is int else 0,
                         "chapters": chapters}
    return {"quiz": quiz, "learners": learners}


def save(data):
    tmp = PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f)
    os.replace(tmp, PATH)


def chapter_view(ch):
    # What a chapter looks like in a learner's record, with the status derived:
    # completed once any attempt passed, in-progress once opened or attempted.
    attempts = ch["attempts"]
    passed = [a for a in attempts if a["passed"]]
    best = max(attempts, key=lambda a: (a["passed"], a["points"], a["tasksRight"])) if attempts else None
    status = "completed" if passed else "in-progress" if (ch["openedAt"] or attempts) else "open"
    return {"status": status, "seconds": ch["seconds"], "openedAt": ch["openedAt"], "lastAt": ch["lastAt"],
            "completedAt": min(a["at"] for a in passed) if passed else None,
            "attempts": attempts, "best": best}


def learner_view(key, rec):
    return {"key": key, "name": rec["name"], "createdAt": rec["createdAt"],
            "chapters": {cid: chapter_view(ch) for cid, ch in rec["chapters"].items()}}


def public_summary(data):
    # The quiz page reads count, max, and scores at the top level; the chapter
    # statistics sit beside them, one entry per attempt and no name.
    chapters = {}
    for rec in data["learners"].values():
        for cid, ch in rec["chapters"].items():
            entries = chapters.setdefault(cid, [])
            entries.extend({"points": a["points"], "passed": a["passed"]} for a in ch["attempts"])
    return {"count": len(data["quiz"]), "max": MAX_POINTS, "scores": data["quiz"],
            "chapters": {cid: {"count": len(e), "max": MAX_POINTS, "passed": sum(1 for x in e if x["passed"]), "scores": e}
                         for cid, e in chapters.items()}}


def empty_chapter():
    return {"seconds": 0, "openedAt": None, "lastAt": None, "attempts": []}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        # No request log: who did what is in the records alone.
        pass

    def reply(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length > 512:
            return None
        return json.loads(self.rfile.read(length) or b"{}")

    def do_GET(self):
        if self.path == "/api/results":
            with LOCK:
                data = load()
            return self.reply(200, public_summary(data))
        if self.path == "/api/admin/learners":
            return self.get_admin()
        m = re.match(r"^/api/learners/([^/]+)$", self.path)
        if m:
            key = learner_key(m.group(1).replace("%20", " "))
            with LOCK:
                data = load()
            rec = data["learners"].get(key)
            if not rec:
                return self.reply(404, {"error": "no such learner"})
            return self.reply(200, learner_view(key, rec))
        return self.reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path == "/api/results":
            return self.post_quiz()
        if self.path == "/api/learners":
            return self.post_learner()
        m = re.match(r"^/api/learners/([^/]+)/chapters/([^/]+)/(open|time|attempts)$", self.path)
        if m:
            return self.post_chapter(learner_key(m.group(1).replace("%20", " ")), m.group(2), m.group(3))
        return self.reply(404, {"error": "not found"})

    def post_quiz(self):
        try:
            body = self.read_body()
            if body is None:
                return self.reply(413, {"error": "body too large"})
            score = body["score"]
        except (ValueError, KeyError, TypeError):
            return self.reply(400, {"error": "expected {\"score\": <integer>}"})
        if not valid_points(score):
            return self.reply(400, {"error": "score must be an odd integer between -75 and 75"})
        with LOCK:
            data = load()
            data["quiz"].append(score)
            save(data)
        return self.reply(201, public_summary(data))

    def post_learner(self):
        try:
            body = self.read_body()
            if body is None:
                return self.reply(413, {"error": "body too large"})
            name = " ".join(str(body["name"]).split())
        except (ValueError, KeyError, TypeError):
            return self.reply(400, {"error": "expected {\"name\": \"<name>\"}"})
        key = learner_key(name)
        if not key or len(name) > 60:
            return self.reply(400, {"error": "a name is 1 to 60 characters of letters, digits, and spaces"})
        with LOCK:
            data = load()
            rec = data["learners"].get(key)
            if not rec:
                if len(data["learners"]) >= MAX_LEARNERS:
                    return self.reply(400, {"error": "too many learners"})
                rec = {"name": name, "createdAt": int(time.time()), "chapters": {}}
                data["learners"][key] = rec
                save(data)
        return self.reply(200, learner_view(key, rec))

    def post_chapter(self, key, cid, what):
        if not CHAPTER_ID.match(cid) or len(cid) > 64:
            return self.reply(404, {"error": "not found"})
        try:
            body = self.read_body()
            if body is None:
                return self.reply(413, {"error": "body too large"})
        except (ValueError, TypeError):
            return self.reply(400, {"error": "expected a JSON object"})
        now = int(time.time())
        with LOCK:
            data = load()
            rec = data["learners"].get(key)
            if not rec:
                return self.reply(404, {"error": "no such learner"})
            ch = rec["chapters"].setdefault(cid, empty_chapter())
            if what == "open":
                ch["openedAt"] = ch["openedAt"] or now
            elif what == "time":
                seconds = body.get("seconds")
                if type(seconds) is not int or seconds < 1 or seconds > MAX_SECONDS_PER_POST:
                    return self.reply(400, {"error": "seconds must be an integer between 1 and %d" % MAX_SECONDS_PER_POST})
                ch["seconds"] += seconds
                ch["openedAt"] = ch["openedAt"] or now
            else:
                attempt = {"points": body.get("points"), "tasksRight": body.get("tasksRight"), "tasksTotal": body.get("tasksTotal"),
                           "passed": body.get("passed"), "at": now}
                if not valid_attempt(attempt):
                    return self.reply(400, {"error": "expected points (odd integer between -75 and 75), tasksRight, tasksTotal, and passed"})
                if len(ch["attempts"]) >= MAX_ATTEMPTS:
                    return self.reply(400, {"error": "too many attempts"})
                ch["attempts"].append(attempt)
                ch["openedAt"] = ch["openedAt"] or now
            ch["lastAt"] = now
            save(data)
        return self.reply(201, chapter_view(ch))

    def get_admin(self):
        if not ADMIN_TOKEN:
            return self.reply(503, {"error": "no admin token is configured"})
        if self.headers.get("X-Admin-Token", "") != ADMIN_TOKEN:
            return self.reply(403, {"error": "wrong token"})
        with LOCK:
            data = load()
        return self.reply(200, {"learners": [learner_view(k, r) for k, r in sorted(data["learners"].items())]})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
