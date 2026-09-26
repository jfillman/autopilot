import pytest
from dataclasses import replace

from clearance.limits import Claim, ComputeClass, Limits, Network, WideningError, narrow
from clearance.tiers import Tier

BASE = Limits(Tier.T1, 30, 200, 300, 3, 500_000, 2, frozenset({"a", "b", "c"}), frozenset({"m1", "m2"}),
              Network.CLEARANCE_MODEL, ComputeClass.MEDIUM)


def test_empty_claim_inherits_everything():
    assert narrow(BASE, Claim()) == BASE


def test_narrowing_applies():
    out = narrow(BASE, Claim(tier_ceiling=Tier.T0, ttl_minutes=10, tool_calls=50, tools=frozenset({"a"}),
                             network=Network.NONE, compute=ComputeClass.SMALL, models=frozenset({"m1"})))
    assert out.tier_ceiling == Tier.T0 and out.ttl_minutes == 10 and out.tool_calls == 50
    assert out.tools == {"a"} and out.network == Network.NONE and out.compute == ComputeClass.SMALL
    assert out.models == {"m1"}
    assert out.github_calls == 300  # untouched


@pytest.mark.parametrize("claim,field", [
    (Claim(tier_ceiling=Tier.T2), "tier_ceiling"),
    (Claim(ttl_minutes=31), "ttl_minutes"),
    (Claim(tool_calls=201), "tool_calls"),
    (Claim(github_calls=301), "github_calls"),
    (Claim(open_prs=4), "open_prs"),
    (Claim(model_tokens=500_001), "model_tokens"),
    (Claim(children=3), "children"),
    (Claim(network=Network.ALLOWLIST), "network"),
    (Claim(compute=ComputeClass.GPU), "compute"),
])
def test_every_field_refuses_to_widen(claim, field):
    with pytest.raises(WideningError) as e:
        narrow(BASE, claim)
    assert field in e.value.fields


def test_tool_and_model_supersets_refuse():
    with pytest.raises(WideningError) as e:
        narrow(BASE, Claim(tools=frozenset({"a", "z"}), models=frozenset({"m9"})))
    assert "tools:z" in e.value.fields and "models:m9" in e.value.fields


def test_all_widening_fields_reported_together():
    with pytest.raises(WideningError) as e:
        narrow(BASE, Claim(ttl_minutes=99, tool_calls=999))
    assert set(e.value.fields) == {"ttl_minutes", "tool_calls"}


def test_equal_is_allowed():
    assert narrow(BASE, Claim(ttl_minutes=30, tools=frozenset(BASE.tools))) == BASE


def test_from_dict_rejects_unknown_fields():
    with pytest.raises(ValueError):
        Claim.from_dict({"tier": "T2"})
    assert Claim.from_dict({"network": "clearance+model", "tier_ceiling": "T0"}).network == Network.CLEARANCE_MODEL
