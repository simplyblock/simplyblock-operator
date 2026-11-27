#!/usr/bin/env python3
# The results API of the operator CRD quiz (crd-quiz.html, beside this file).
# GET /api/results returns every point total sent in so far, and POST
# /api/results appends one. The store is a JSON list of integers and nothing
# else, which is what keeps the statistics anonymous: no name, address, or time
# is ever written, and the server keeps no request log either.
#
# It is a separate file rather than a block inside crd-quiz-host.sh so that it
# can be read, checked, and run on its own. The host script downloads it at boot
# and runs it as a systemd service behind nginx, which adds the request limit.
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PATH = os.environ.get("STATE_DIRECTORY", "/var/lib/crd-quiz") + "/results.json"
MAX_POINTS = 75
LOCK = threading.Lock()


def load():
    try:
        with open(PATH, encoding="utf-8") as f:
            scores = json.load(f)
        return [s for s in scores if isinstance(s, int)]
    except (OSError, ValueError):
        return []


def save(scores):
    tmp = PATH + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(scores, f)
    os.replace(tmp, PATH)


def summary(scores):
    return {"count": len(scores), "max": MAX_POINTS, "scores": scores}


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
            scores = load()
        return self.reply(200, summary(scores))

    def do_POST(self):
        if self.path != "/api/results":
            return self.reply(404, {"error": "not found"})
        length = int(self.headers.get("Content-Length") or 0)
        if length > 256:
            return self.reply(413, {"error": "body too large"})
        try:
            score = json.loads(self.rfile.read(length) or b"{}")["score"]
        except (ValueError, KeyError, TypeError):
            return self.reply(400, {"error": "expected {\"score\": <integer>}"})
        # A quiz scores 2 * right - 75 over 75 judged answers, so a total is an
        # odd integer between -75 and 75. Anything else did not come from the page.
        if type(score) is not int or score < -MAX_POINTS or score > MAX_POINTS or (score + MAX_POINTS) % 2:
            return self.reply(400, {"error": "score must be an odd integer between -75 and 75"})
        with LOCK:
            scores = load()
            scores.append(score)
            save(scores)
        return self.reply(201, summary(scores))


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
