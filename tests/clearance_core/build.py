"""Build autopilot's test core: the clearance binary from a clearance checkout, with testcore.go and testkinds.go
overlaid (go build -overlay; the checkout is not modified).

    python tests/clearance_core/build.py <clearance checkout> <output binary>

CI checks clearance out at the SHA pyproject.toml pins the client by, so the core under test is the core the client
was generated from.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent


def build(src: Path, out: Path) -> Path:
    src, out = src.resolve(), out.resolve()
    overlay = {"Replace": {str(src / "cmd" / "clearance" / "zz_autopilot_testcore.go"): str(HERE / "testcore.go"),
                           str(src / "internal" / "server" / "zz_autopilot_testkinds.go"): str(HERE / "testkinds.go")}}
    with tempfile.TemporaryDirectory() as d:
        # The overlaid files carry `//go:build ignore` (so autopilot's tree is not a Go package); the overlay copies
        # drop that line.
        repl = {}
        for dst, orig in overlay["Replace"].items():
            body = Path(orig).read_text().replace("//go:build ignore\n", "", 1)
            p = Path(d) / Path(dst).name
            p.write_text(body)
            repl[dst] = str(p)
        ov = Path(d) / "overlay.json"
        ov.write_text(json.dumps({"Replace": repl}))
        out.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(["go", "build", "-trimpath", "-overlay", str(ov), "-o", str(out), "./cmd/clearance"],
                       cwd=src, check=True, env={**os.environ, "CGO_ENABLED": "0"})
    return out


if __name__ == "__main__":
    print(build(Path(sys.argv[1]), Path(sys.argv[2])))
