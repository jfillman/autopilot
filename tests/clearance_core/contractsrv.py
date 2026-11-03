"""A stand-in for glidepath-api's GET /v1/contract, which the test core's Hangar bundle reads at start: it serves
glidepath's contract (the checkout under HANGAR_TECH when there is one, as CI clones it; else
tests/fixtures/glidepath-contract.json) and, like glidepath-api, refuses a request without a bearer token."""
from __future__ import annotations

import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "tests" / "fixtures" / "glidepath-contract.json"
TOKEN = "contract-reader-token"


def contract_path() -> Path:
    tech = os.environ.get("HANGAR_TECH")
    if tech and (Path(tech) / "glidepath" / "contract" / "glidepath-contract.json").exists():
        return Path(tech) / "glidepath" / "contract" / "glidepath-contract.json"
    return FIXTURE


class ContractServer:
    def __init__(self, body: bytes):
        self.body, self.hits = body, []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_GET(self):
                outer.hits.append((self.path, self.headers.get("Authorization")))
                if self.path != "/v1/contract":
                    self.send_error(404)
                elif self.headers.get("Authorization") != f"Bearer {TOKEN}":
                    self.send_error(401)
                else:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(outer.body)))
                    self.end_headers()
                    self.wfile.write(outer.body)

            def log_message(self, *a):
                pass

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}/v1/contract"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def stop(self):
        self.srv.shutdown()
