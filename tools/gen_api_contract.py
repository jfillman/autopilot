#!/usr/bin/env python3
"""Write contract/autopilot-api.json from autopilot.read_api.ROUTES (docs/admin/README.md "The read API").

    python3 tools/gen_api_contract.py           write it
    python3 tools/gen_api_contract.py --check   exit 1 if the committed file is stale (tests/test_read_api.py)
"""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
from autopilot.read_api import contract  # noqa: E402

OUT = ROOT / "contract" / "autopilot-api.json"


def render() -> str:
    return json.dumps(contract(), indent=2, sort_keys=False) + "\n"


if __name__ == "__main__":
    if "--check" in sys.argv:
        if not OUT.exists() or OUT.read_text() != render():
            print(f"{OUT.relative_to(ROOT)} is stale: run python3 tools/gen_api_contract.py and commit it")
            sys.exit(1)
        print(f"{OUT.relative_to(ROOT)} is current")
    else:
        OUT.parent.mkdir(exist_ok=True)
        OUT.write_text(render())
        print(f"wrote {OUT.relative_to(ROOT)}")
