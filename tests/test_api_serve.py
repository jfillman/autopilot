"""`autopilot-gateway api`: clearance-api built from the gateway's environment, with Tower's catalog as the
Directory and Ownership."""
import json

import pytest
from starlette.testclient import TestClient

from autopilot import api_serve, serve
from autopilot.reads import BackstageCatalog, BackstageDirectory, BackstageOwnership

EPH = {"AIRFRAME_ENABLED": "0"}


def test_needs_its_own_dsn():
    with pytest.raises(serve.ConfigError, match="CLEARANCE_API_DSN"):
        api_serve.build(EPH)


def test_builds_with_roles_and_catalog(tmp_path):
    tok = tmp_path / "t"
    tok.write_text("x")
    cfg, access, app = api_serve.build({**EPH, "CLEARANCE_API_DSN": "postgresql://r@pg/clearance",
                                        "CLEARANCE_API_AUDITORS": "user:jfillman", "CLEARANCE_API_NOTIFIERS": "hangar-prod/*/backstage-ingestor",
                                        "CLEARANCE_DELEGATES": "hangar-prod/app-backstage-prod/backstage-ingestor",
                                        "BACKSTAGE_URL": "http://bs", "BACKSTAGE_TOKEN_FILE": str(tok),
                                        "CLEARANCE_GATEWAY_URL": "http://clearance.hangar-dev.local"})
    assert access.auditors == ("user:jfillman",) and access.notifiers == ("hangar-prod/*/backstage-ingestor",)
    assert isinstance(access.directory, BackstageDirectory) and isinstance(access.ownership, BackstageOwnership)
    assert all(getattr(d, "owner", None) for d in access.definitions.values()), "every Hangar agent names its team"
    with TestClient(app) as c:
        assert c.get("/healthz").json() == {"ok": True}
        assert c.get("/v1/contract").json()["gateway"] == "http://clearance.hangar-dev.local"
        assert c.get("/v1/sessions").status_code == 401


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
