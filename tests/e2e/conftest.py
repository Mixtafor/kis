import os
import time
import uuid
from dataclasses import dataclass

import httpx
import pytest


@dataclass
class API:
    client: httpx.Client
    token: str = ""

    def request(self, method, path, *, expected, **kwargs):
        headers = dict(kwargs.pop("headers", {}))
        if self.token:
            headers.setdefault("Authorization", f"Bearer {self.token}")
        response = self.client.request(method, path, headers=headers, **kwargs)
        assert response.status_code == expected, (
            f"{method} {path}: expected {expected}, got {response.status_code}: {response.text[:1000]}"
        )
        return response

    def get(self, path, **kwargs):
        return self.request("GET", path, expected=kwargs.pop("expected", 200), **kwargs)

    def post(self, path, **kwargs):
        return self.request("POST", path, expected=kwargs.pop("expected", 200), **kwargs)

    def login(self, client_id, api_key):
        session = self.post(
            "/v1/handshake", json={"client_id": client_id, "api_key": api_key}
        ).json()
        return API(self.client, session["token"])


@pytest.fixture(scope="session")
def public_api():
    url = os.environ.get("EDITION_URL", "http://localhost:8080").rstrip("/")
    timeout = float(os.environ.get("E2E_READY_TIMEOUT", "60"))
    with httpx.Client(base_url=url, timeout=15, trust_env=False) as client:
        deadline = time.monotonic() + timeout
        last = "no response"
        while time.monotonic() < deadline:
            try:
                response = client.get("/health/ready", timeout=2)
                if response.status_code == 200:
                    break
                last = f"HTTP {response.status_code}: {response.text[:200]}"
            except httpx.HTTPError as error:
                last = str(error)
            time.sleep(0.2)
        else:
            pytest.fail(f"Service {url} was not ready after {timeout}s: {last}")
        yield API(client)


@pytest.fixture(scope="session")
def editor(public_api):
    key = os.environ.get("EDITOR_API_KEY")
    if not key:
        pytest.fail("Set EDITOR_API_KEY to the configured editor key (see .env.example)")
    return public_api.login(os.environ.get("EDITOR_CLIENT_ID", "editor"), key)


@pytest.fixture
def policy(editor):
    original = editor.get("/v1/editor/policy").json()

    def set_policy(threshold=1.1, half_life_seconds=21600, congratulations_count=3):
        value = dict(
            threshold=threshold,
            half_life_seconds=half_life_seconds,
            congratulations_count=congratulations_count,
        )
        return editor.request("PUT", "/v1/editor/policy", expected=200, json=value).json()

    try:
        yield set_policy
    finally:
        editor.request("PUT", "/v1/editor/policy", expected=200, json=original)


@pytest.fixture
def provision(editor, public_api):
    def create(trust=0.7, role="staff"):
        client_id = "e2e-" + uuid.uuid4().hex
        response = editor.post(
            "/v1/editor/clients", expected=201,
            json={"id": client_id, "name": client_id, "role": role, "trust": trust, "enabled": True},
        ).json()
        return response, public_api.login(client_id, response["api_key"])

    return create
