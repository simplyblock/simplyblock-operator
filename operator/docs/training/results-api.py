#!/usr/bin/env python3
# The results API of the operator training: the anonymous statistics of the
# CRD quiz (crd-quiz.html) and of every chapter's test (chapter.html).
#
#   GET  /api/results            every statistic: the quiz's point totals, and
#                                per chapter the points and pass of each test
#   POST /api/results            one finished quiz, {"score": <int>}
#   POST /api/results/<chapter>  one finished chapter test,
#                                {"points": <int>, "passed": <bool>}
#
# The store is one JSON file holding integers and booleans and nothing else,
# which is what keeps it anonymous: no name, address, or time is ever written,
# and the server keeps no request log either. It is a separate file rather than
# a block inside training-host.sh so that it can be read, checked, and run on
# its own; the host script runs it as a systemd service behind nginx, which adds
# the request limit.
import json
import os
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PATH = os.environ.get("STATE_DIRECTORY", "/var/lib/crd-quiz") + "/results.json"
# A quiz or a chapter's questions score 2 * right - 75 over 75 judged answers,
# so a total is an odd integer between -75 and 75. Anything else did not come
# from the page.
MAX_POINTS = 75
CHAPTER_ID = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")
MAX_CHAPTERS = 64
LOCK = threading.Lock()


def valid_points(value):
    return type(value) is int and -MAX_POINTS <= value <= MAX_POINTS and (value + MAX_POINTS) % 2 == 0


def load():
    try:
        with open(PATH, encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, ValueError):
        return {"quiz": [], "chapters": {}}
    # The quiz alone wrote a bare list before the chapters existed.
    if isinstance(data, list):
        data = {"quiz": data, "chapters": {}}
    quiz = [s for s in data.get("quiz", []) if valid_points(s)]
    chapters = {}
    for cid, entries in data.get("chapters", {}).items():
        if CHAPTER_ID.match(cid) and isinstance(entries, list):
            chapters[cid] = [e for e in entries if isinstance(e, dict) and valid_points(e.get("points")) and type(e.get("passed")) is bool]
    return {"quiz": quiz, "chapters": chapters}


def save(data):
    tmp = PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f)
    os.replace(tmp, PATH)


def chapter_summary(entries):
    return {"count": len(entries), "max": MAX_POINTS, "passed": sum(1 for e in entries if e["passed"]),
            "scores": [{"points": e["points"], "passed": e["passed"]} for e in entries]}


def summary(data):
    # The quiz page reads count, max, and scores at the top level; the chapters
    # sit beside them.
    return {"count": len(data["quiz"]), "max": MAX_POINTS, "scores": data["quiz"],
            "chapters": {cid: chapter_summary(entries) for cid, entries in data["chapters"].items()}}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        # No request log: who sent a result is not kept anywhere.
        pass

    def reply(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path != "/api/results":
            return self.reply(404, {"error": "not found"})
        with LOCK:
            data = load()
        return self.reply(200, summary(data))

    def read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length > 256:
            return None
        return json.loads(self.rfile.read(length) or b"{}")

    def do_POST(self):
        if self.path == "/api/results":
            return self.post_quiz()
        if self.path.startswith("/api/results/"):
            return self.post_chapter(self.path[len("/api/results/"):])
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
        return self.reply(201, summary(data))

    def post_chapter(self, cid):
        if not CHAPTER_ID.match(cid) or len(cid) > 64:
            return self.reply(404, {"error": "not found"})
        try:
            body = self.read_body()
            if body is None:
                return self.reply(413, {"error": "body too large"})
            points, passed = body["points"], body["passed"]
        except (ValueError, KeyError, TypeError):
            return self.reply(400, {"error": "expected {\"points\": <integer>, \"passed\": <boolean>}"})
        if not valid_points(points) or type(passed) is not bool:
            return self.reply(400, {"error": "points must be an odd integer between -75 and 75, and passed a boolean"})
        with LOCK:
            data = load()
            entries = data["chapters"].setdefault(cid, [])
            if len(data["chapters"]) > MAX_CHAPTERS:
                return self.reply(400, {"error": "too many chapters"})
            entries.append({"points": points, "passed": passed})
            save(data)
        return self.reply(201, chapter_summary(entries))


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
