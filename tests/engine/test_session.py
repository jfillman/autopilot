import random
from datetime import timedelta

import pytest

from clearance.limits import BUDGET_FIELDS, Claim, ComputeClass, Limits, Network, WideningError
from clearance.session import DENIAL_TRIP, MAX_DEPTH, SessionError, SessionStore
from clearance.tiers import Tier

from conftest import T0

BASE = Limits(Tier.T1, 60, 100, 100, 5, 1000, 3, frozenset({"a", "b"}), frozenset({"m"}),
              Network.CLEARANCE_MODEL, ComputeClass.MEDIUM)


def store():
    n = iter(range(1, 999))
    return SessionStore(id_gen=lambda: f"s-{next(n):06x}")


def test_budgets_spend_and_exhaust():
    s = store().open("u", "a", BASE, None, T0)
    s.spend("tool_calls", 100)
    with pytest.raises(SessionError):
        s.spend("tool_calls", 1)


def test_expiry_and_ttl():
    s = store().open("u", "a", BASE, Claim(ttl_minutes=10), T0)
    assert not s.expired(T0 + timedelta(minutes=9, seconds=59))
    assert s.expired(T0 + timedelta(minutes=10))
    assert s.minutes_left(T0 + timedelta(minutes=4)) == 6


def test_child_is_narrower_and_reserves_from_parent():
    st = store()
    p = st.open("u", "orch", BASE, None, T0)
    c = st.open("u", "res", BASE, Claim(tool_calls=40, model_tokens=300), T0, parent_id=p.id)
    assert c.limits.tool_calls == 40 and c.parent_id == p.id and c.task_id == p.task_id
    assert p.remaining("tool_calls") == 60 and p.remaining("model_tokens") == 700
    assert p.remaining("children") == 2


def test_children_cannot_collectively_exceed_the_parent():
    st = store()
    p = st.open("u", "orch", BASE, None, T0)
    st.open("u", "x", BASE, Claim(tool_calls=70), T0, parent_id=p.id)
    # Second child asks for 70 but only 30 remain: it is clamped to what is left, never above.
    c2 = st.open("u", "x", BASE, None, T0, parent_id=p.id)
    assert c2.limits.tool_calls == 30 and p.remaining("tool_calls") == 0


def test_child_cannot_widen_beyond_parent_remaining():
    st = store()
    p = st.open("u", "orch", BASE, Claim(tool_calls=50), T0)
    with pytest.raises(WideningError):
        st.open("u", "x", BASE, Claim(tool_calls=60), T0, parent_id=p.id)


def test_child_ttl_never_outlives_parent():
    st = store()
    p = st.open("u", "orch", BASE, Claim(ttl_minutes=20), T0)
    c = st.open("u", "x", BASE, None, T0 + timedelta(minutes=5), parent_id=p.id)
    assert c.expires_at == p.expires_at


def test_child_inherits_narrower_tier_tools_network():
    st = store()
    p = st.open("u", "orch", BASE, Claim(tier_ceiling=Tier.T0, tools=frozenset({"a"}), network=Network.CLEARANCE), T0)
    c = st.open("u", "x", BASE, None, T0, parent_id=p.id)
    assert c.limits.tier_ceiling == Tier.T0 and c.limits.tools == {"a"} and c.limits.network == Network.CLEARANCE


def test_children_budget_and_depth_are_bounded():
    st = store()
    p = st.open("u", "orch", BASE, Claim(children=1), T0)
    st.open("u", "x", BASE, None, T0, parent_id=p.id)
    with pytest.raises(SessionError):
        st.open("u", "x", BASE, None, T0, parent_id=p.id)
    cur = st.open("u", "d0", BASE, None, T0)
    for _ in range(MAX_DEPTH):
        cur = st.open("u", "d", BASE, None, T0, parent_id=cur.id)
    with pytest.raises(SessionError):
        st.open("u", "d", BASE, None, T0, parent_id=cur.id)


def test_closing_a_child_returns_unspent_budget_and_charges_spent():
    st = store()
    p = st.open("u", "orch", BASE, None, T0)
    c = st.open("u", "x", BASE, Claim(tool_calls=40), T0, parent_id=p.id)
    c.spend("tool_calls", 10)
    st.close(c.id)
    assert p.remaining("tool_calls") == 90 and p.used["tool_calls"] == 10


def test_closing_a_parent_closes_the_tree():
    st = store()
    p = st.open("u", "orch", BASE, None, T0)
    c = st.open("u", "x", BASE, None, T0, parent_id=p.id)
    g = st.open("u", "x", BASE, None, T0, parent_id=c.id)
    st.close(p.id)
    assert p.closed and c.closed and g.closed


def test_tripping_a_parent_trips_the_children_and_blocks_new_ones():
    st = store()
    p = st.open("u", "orch", BASE, None, T0)
    c = st.open("u", "x", BASE, None, T0, parent_id=p.id)
    st.trip(p.id, "test")
    assert c.tripped
    with pytest.raises(SessionError):
        st.open("u", "x", BASE, None, T0, parent_id=p.id)


def test_repeated_denials_trip_the_breaker():
    s = store().open("u", "a", BASE, None, T0)
    for _ in range(DENIAL_TRIP - 1):
        s.record_denial()
    assert not s.tripped
    s.record_denial()
    assert s.tripped and "denied" in s.trip_reason


def test_sweep_closes_expired_sessions():
    st = store()
    a = st.open("u", "a", BASE, Claim(ttl_minutes=5), T0)
    b = st.open("u", "b", BASE, Claim(ttl_minutes=50), T0)
    assert st.sweep(T0 + timedelta(minutes=6)) == [a.id]
    assert a.closed and not b.closed


def test_budget_conservation_under_random_operations():
    """Whatever happens, spent + reserved + remaining never exceeds what the root was granted."""
    rnd = random.Random(7)
    for trial in range(200):
        st = store()
        root = st.open("u", "r", BASE, None, T0)
        live = [root]
        for _ in range(30):
            act = rnd.choice(["spawn", "spend", "close"])
            s = rnd.choice(live)
            try:
                if act == "spawn" and not s.closed:
                    claim = Claim(tool_calls=rnd.randint(1, 60), model_tokens=rnd.randint(1, 600))
                    live.append(st.open("u", "x", BASE, claim, T0, parent_id=s.id))
                elif act == "spend" and not s.closed:
                    s.spend("tool_calls", rnd.randint(1, 10))
                elif act == "close":
                    st.close(s.id)
            except (SessionError, WideningError):
                pass
            for f in BUDGET_FIELDS:
                for x in live:
                    assert x.remaining(f) >= 0, (trial, f)
                    assert x.used[f] + x.reserved[f] <= getattr(x.limits, f), (trial, f)
