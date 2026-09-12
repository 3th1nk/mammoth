#!/usr/bin/env python3
"""Catch server for the probe ISO qemu loop: accepts the probe's POST at any
path, dumps the body to --out, answers 200. Runs until interrupted.

Usage: report-server.py --port 8765 --out /tmp/probe-dev-report.json
"""
import argparse
from http.server import BaseHTTPRequestHandler, HTTPServer


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--out", default="/tmp/probe-dev-report.json")
    args = ap.parse_args()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length)
            with open(args.out, "wb") as f:
                f.write(body)
            print(f"[report-server] {self.path} <- {length} bytes -> {args.out}", flush=True)
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")

        def log_message(self, *a):
            pass

    HTTPServer(("0.0.0.0", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
