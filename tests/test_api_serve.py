"""`autopilot-gateway api`: the gateway's read API (read_api.py) built from the gateway's environment, with Tower's
catalog as the Directory and Ownership."""
import json

import pytest
from starlette.testclient import TestClient

from autopilot import api_serve, read_api, serve
from autopilot.reads import BackstageCatalog, BackstageDirectory, BackstageOwnership

EPH = {"AIRFRAME_ENABLED": "0"}


def test_needs_its_own_dsn():
    with pytest.raises(serve.ConfigError, match="GATEWAY_API_DSN"):
        api_serve.build(EPH)
    for bad, msg in (({"GATEWAY_API_TABLES": "clearance"}, "GATEWAY_API_TABLES is retired"),
                     ({"GATEWAY_API_TABLES": "evidence"}, "GATEWAY_API_TABLES"), ({"GATEWAY_API_POOL": "x"}, "numbers"),
                     ({"GATEWAY_API_POOL": "99"}, "1 to 50")):
        with pytest.raises(serve.ConfigError, match=msg):
            api_serve.build({**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway", **bad})


def test_builds_with_roles_and_catalog(tmp_path):
    tok = tmp_path / "t"
    tok.write_text("x")
    cfg, access, app = api_serve.build({**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway",
                                        "GATEWAY_API_AUDITORS": "user:jfillman", "GATEWAY_API_NOTIFIERS": "hangar-prod/*/backstage-ingestor",
                                        "AUTOPILOT_DELEGATES": "hangar-prod/app-backstage-prod/backstage-ingestor",
                                        "BACKSTAGE_URL": "http://bs", "BACKSTAGE_TOKEN_FILE": str(tok),
                                        "AUTOPILOT_GATEWAY_URL": "http://clearance.hangar-dev.local",
                                        "CLEARANCE_API_URL": "http://clearance-api.hangar-dev.local"})
    assert access.auditors == ("user:jfillman",) and access.notifiers == ("hangar-prod/*/backstage-ingestor",)
    assert isinstance(access.directory, BackstageDirectory) and isinstance(access.ownership, BackstageOwnership)
    assert all(getattr(d, "owner", None) for d in access.definitions.values()), "every Hangar agent names its team"
    with TestClient(app) as c:
        assert c.get("/healthz").json() == {"ok": True}
        contract = c.get("/v1/contract").json()
        assert contract["api"] == "autopilot-api" and contract["gateway"] == "http://clearance.hangar-dev.local"
        assert contract["clearance"] == "http://clearance-api.hangar-dev.local", "Clearance's reads are named"
        assert c.get("/v1/sessions").status_code == 401
        assert c.get("/v1/evidence").status_code == 404, "evidence is clearance-api's"


def test_the_deployed_tables_value_is_accepted_and_reads_the_gateways_tables(monkeypatch):
    """R-4: GATEWAY_API_TABLES=gateway, as the Deployment still sets it, starts the API for one release; the store reads
    gateway_* whether it is set or not."""
    stores = []

    class Recording(read_api.PgReadStore):
        def __init__(self, *a, **k):
            super().__init__(*a, **k)
            stores.append(self)
    monkeypatch.setattr(read_api, "PgReadStore", Recording)
    for extra in ({"GATEWAY_API_TABLES": "gateway"}, {}):
        _, _, app = api_serve.build({**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway", **extra})
        assert app is not None
    assert len(stores) == 2
    for store in stores:
        assert all(t.startswith("gateway_") for t in store.t.values()), store.t


def test_the_pre_r3c_names_no_longer_configure_it():
    """The CLEARANCE_API_* fallbacks are gone (R-4): nothing deployed sets them, so they configure nothing."""
    with pytest.raises(serve.ConfigError, match="GATEWAY_API_DSN"):
        api_serve.build({**EPH, "CLEARANCE_API_DSN": "postgresql://r@pg/clearance"})
    _, access, _ = api_serve.build({**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway",
                                    "CLEARANCE_API_AUDITORS": "user:old", "CLEARANCE_API_NOTIFIERS": "ns/old"})
    assert access.auditors == () and access.notifiers == ()


def test_ownership_is_the_components_you_or_your_teams_own(tmp_path):
    tok = tmp_path / "t"
    tok.write_text("x")
    urls = []

    def opener(req, timeout):
        urls.append(req.full_url)
        if "/by-name/user/" in req.full_url:
            return 200, json.dumps({"relations": [{"type": "memberOf", "targetRef": "group:default/team-skyport"}]}).encode()
        return 200, json.dumps({"items": [{"metadata": {"name": "gate-api"}}, {"metadata": {"name": "Flight-API"}}]}).encode()
    cat = BackstageCatalog("http://bs", tok, opener=opener)
    own = BackstageOwnership(cat, BackstageDirectory(cat))
    assert own.apps("user:jfillman") == {"gate-api", "flight-api"}
    q = urls[-1]
    assert "spec.owner%3Duser%3Adefault%2Fjfillman" in q and "spec.owner%3Dgroup%3Adefault%2Fteam-skyport" in q
    assert own.apps("ns/sa") == set() and own.apps("user:../x") == set()


def test_the_read_apis_gateway_url_has_a_legacy_alias(caplog):
    base = {**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway"}
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        *_, app = api_serve.build({**base, "CLEARANCE_GATEWAY_URL": "http://old"})
    assert any("CLEARANCE_GATEWAY_URL -> AUTOPILOT_GATEWAY_URL" in r.getMessage() for r in caplog.records)
    with TestClient(app) as c:
        assert c.get("/v1/contract").json()["gateway"] == "http://old"
    *_, app = api_serve.build({**base, "CLEARANCE_GATEWAY_URL": "http://old", "AUTOPILOT_GATEWAY_URL": "http://new"})
    with TestClient(app) as c:
        assert c.get("/v1/contract").json()["gateway"] == "http://new", "the new name wins"


def test_the_read_api_does_not_warn_about_names_it_sets_itself(caplog):
    with caplog.at_level("WARNING", logger="autopilot.gateway"):
        api_serve.build({**EPH, "GATEWAY_API_DSN": "postgresql://r@pg/gateway"})
    assert "deprecated" not in caplog.text
