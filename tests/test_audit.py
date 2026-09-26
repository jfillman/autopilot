import json

import pytest

from clearance.audit import AuditLog, args_digest, verify, verify_against


def fill(n=5):
    log = AuditLog()
    for i in range(n):
        log.append({"tool": f"t{i}", "decision": "allow"})
    return log


def test_chain_verifies():
    assert verify(fill().records) == (True, None)


def test_edit_is_detected():
    log = fill()
    log.records[2]["decision"] = "deny"
    assert verify(log.records) == (False, 2)


def test_delete_is_detected():
    log = fill()
    del log.records[2]
    ok, bad = verify(log.records)
    assert not ok and bad == 2


def test_reorder_is_detected():
    log = fill()
    log.records[1], log.records[2] = log.records[2], log.records[1]
    assert not verify(log.records)[0]


def test_truncation_is_a_valid_chain_but_fails_the_anchor():
    log = fill(6)
    anchor = log.checkpoint()
    cut = log.records[:4]
    assert verify(cut)[0], "a truncated chain is still internally consistent"
    ok, why = verify_against(cut, anchor)
    assert not ok and "truncated" in why


def test_rewrite_after_anchor_is_detected():
    log = fill(4)
    anchor = log.checkpoint()
    forged = AuditLog()
    for i in range(4):
        forged.append({"tool": "forged", "decision": "allow"})
    assert verify(forged.records)[0]
    ok, why = verify_against(forged.records, anchor)
    assert not ok and "rewritten" in why


def test_growth_after_anchor_is_fine():
    log = fill(3)
    anchor = log.checkpoint()
    log.append({"tool": "later"})
    assert verify_against(log.records, anchor) == (True, "ok")


def test_event_cannot_smuggle_chain_fields():
    with pytest.raises(ValueError):
        AuditLog().append({"hash": "x"})


def test_arguments_are_hashed_never_stored():
    d = args_digest({"password": "hunter2"})
    assert len(d) == 64 and "hunter2" not in d


def test_persists_and_reloads_and_refuses_a_tampered_file(tmp_path):
    p = tmp_path / "audit.jsonl"
    log = AuditLog(p)
    for i in range(3):
        log.append({"tool": f"t{i}"})
    assert len(AuditLog(p).records) == 3
    lines = p.read_text().splitlines()
    rec = json.loads(lines[1]); rec["tool"] = "evil"; lines[1] = json.dumps(rec, sort_keys=True)
    p.write_text("\n".join(lines) + "\n")
    with pytest.raises(ValueError):
        AuditLog(p)
