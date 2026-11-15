"""The environment-variable table of the Go gateway (A-9 S7): gateway/envtable.json is the source (every variable the Python gateway and
autopilot-api read, against the Go gateway's), docs/admin/go-gateway-env.md is its rendering.

  python tools/gen_gateway_env_table.py            rewrite the document
  python tools/gen_gateway_env_table.py --check    exit 1 if the committed document differs (what CI runs)

tests/test_gateway_env_table.py holds the JSON to the Python source (every variable the Python gateway names is in it, and none that
Python does not read is said to be Python's); gateway/cmd/autopilot-gateway/envtable_test.go holds it to the Go source and fails when a
variable the Python Deployment sets is silently ignored by Go.
"""
from __future__ import annotations

import json
import sys
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TABLE = ROOT / "gateway" / "envtable.json"
DOC = ROOT / "docs" / "admin" / "go-gateway-env.md"

ORDER = [
    ("same", "Same name, same meaning (Go may be stricter: see the note)"),
    ("new", "New in Go (the Python gateway has no such setting)"),
    ("refused", "Refused at start by both gateways"),
    ("retired", "Renamed AUTOPILOT_* at R-10a; the old name is refused by both"),
    ("not_carried", "Not carried: Python reads it, Go does not"),
    ("kubelet", "Set on the pod and read by no process"),
]


def render() -> str:
    doc = json.loads(TABLE.read_text())
    rows = doc["vars"]
    n = Counter(r["class"] for r in rows)
    out = [
        "# The Go gateway's environment, against the Python gateway's",
        "",
        "Generated from `gateway/envtable.json` by `tools/gen_gateway_env_table.py` (CI checks it is current); do not edit by hand.",
        "The table is held to the Python source by `tests/test_gateway_env_table.py` and to the Go source by",
        "`gateway/cmd/autopilot-gateway/envtable_test.go`, which also fails the build when a variable the Python Deployment sets today",
        "would be silently ignored by Go.",
        "",
        "Columns: **Python** and **Go** are the processes that read it (`serve` the gateway, `api` the read API, `migrate` the index step);",
        "**Set today** is where the Python Deployment (apron `templates/55-autopilot/gateway`) sets it.",
        "",
        "## Counts",
        "",
        "| Class | Variables |",
        "|---|---|",
    ]
    for cls, title in ORDER:
        out.append(f"| {title} | {n.get(cls, 0)} |")
    out.append(f"| Total | {len(rows)} |")
    out.append("")
    for cls, title in ORDER:
        sel = [r for r in rows if r["class"] == cls]
        if not sel:
            continue
        out += [f"## {title}", "", "| Variable | Python | Go | Set today | Note |", "|---|---|---|---|---|"]
        for r in sorted(sel, key=lambda r: r["name"]):
            note = r["note"].replace("|", "\\|")
            out.append(f"| `{r['name']}` | {r['python'] or '-'} | {r['go'] or '-'} | {r.get('deployed') or '-'} | {note} |")
        out.append("")
    return "\n".join(out)


def main(argv: list[str]) -> int:
    text = render()
    if "--check" in argv:
        if not DOC.is_file() or DOC.read_text() != text:
            print("docs/admin/go-gateway-env.md is out of date: run `python tools/gen_gateway_env_table.py`", file=sys.stderr)
            return 1
        return 0
    DOC.write_text(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
