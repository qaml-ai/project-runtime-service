#!/usr/bin/env bash
set -euo pipefail

python3 - <<'PY' &
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, format, *args):
        return

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
PY

exec sleep infinity
