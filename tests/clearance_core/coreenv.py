"""The test core's settings as a gateway reads them (tests/conftest.py starts the core in pytest_configure): what
a test's environment for serve.config_from_env adds, so the gateway it builds asks the real test core."""
import os


def core_env() -> dict[str, str]:
    return {"CLEARANCE_CORE_URL": os.environ["AUTOPILOT_TEST_CORE_URL"],
            "CLEARANCE_CORE_TOKEN_FILE": os.environ["AUTOPILOT_TEST_CORE_TOKEN_FILE"]}
