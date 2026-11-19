"""Writes pattern_parity.json: for every `pattern` in the vendored AppSpec schema that uses \\s, which of a fixed set of strings
Python's `re.search` (what jsonschema uses for `pattern`) matches. TestPatternParity compiles the same patterns in Go and
requires the same answers. Expectations come from running Python here, never by hand:

    python gateway/internal/definitions/testdata/gen_pattern_parity.py
"""
import json
import re
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCHEMA = HERE.parent.parent / "airframe" / "appspec.schema.json"


def patterns(node, out):
    if isinstance(node, dict):
        p = node.get("pattern")
        if isinstance(p, str) and "\\s" in p:
            out.add(p)
        for v in node.values():
            patterns(v, out)
    elif isinstance(node, list):
        for v in node:
            patterns(v, out)


found: set[str] = set()
patterns(json.loads(SCHEMA.read_text()), found)

NBSP, EM, NEL, IDEO = " ", " ", "\u0085", "　"
SAMPLES = [
    "", " ", "\t", "\n",
    'job="api"', ' job="api" ', '\tjob="api"\t', 'job = "api"', 'job\t=\t"api"',
    'job="api",code="200"', 'job="api" , code="200"', 'job="api",', ',job="api"',
    'job!="api"', 'job=~"a.*"', 'job!~"a.*"', 'job=="api"', 'job=api', 'job="api',
    'namespace="other"', 'namespace="x",job="api"', '__name__="up"', '__name__=~".+"',
    'job="a\\"b"', 'job="a\nb"', 'job="a\\\\"', 'job="a"b"',
    NBSP + 'job="api"', 'job="api"' + NBSP, 'job' + NBSP + '="api"', 'job=' + EM + '"api"',
    NEL + 'job="api"', 'job="api"' + NEL, 'job' + EM + ',x="y"', 'job="api",' + IDEO + 'code="2"',
    'job="a' + NBSP + 'b"', 'job="a' + EM + 'b"',
    '\x1cjob="api"', '\x1fjob="api"', '\x0bjob="api"', '\x0cjob="api"',
    "img@sha256:" + "a" * 64, "img @sha256:" + "a" * 64, "img" + NBSP + "@sha256:" + "a" * 64, "img" + EM + "@sha256:" + "a" * 64,
    "img@sha256:" + "a" * 63,
    "http_requests_total", "1abc", "a b", "a" + NBSP + "b", "a​b", '﻿job="api"',
]
table = [{"pattern": p, "results": {s: bool(re.search(p, s)) for s in SAMPLES}} for p in sorted(found)]
assert table, "no pattern with \\s found: wrong schema path?"
(HERE / "pattern_parity.json").write_text(json.dumps(table, indent=1, ensure_ascii=True) + "\n")
print(len(table), "patterns,", len(SAMPLES), "samples")
