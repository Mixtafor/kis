from datetime import datetime, timedelta, timezone
import uuid


def unique():
    return "e2e-" + uuid.uuid4().hex


def note(category=None, topic=None, *, seconds_ago=0):
    instant = datetime.now(timezone.utc) - timedelta(seconds=seconds_ago)
    return {
        "request_id": unique(),
        "category": category or unique(),
        "topic": topic or unique(),
        "body": "Independent source confirms the event.",
        "event_at": instant.isoformat().replace("+00:00", "Z"),
    }


def submit(api, value, expected=201):
    return api.post("/v1/notes", expected=expected, json=value).json()


def inbox(api):
    return api.get("/v1/messages", params={"unacked": "true"}).json()["items"]


# Проверяем права клиентов и отказ по старому JWT после отключения клиента.
def test_client_access_and_immediate_disable(public_api, editor, provision):
    account, api = provision()
    public_api.get("/v1/stats/me", expected=401)
    public_api.post(
        "/v1/handshake", expected=401,
        json={"client_id": account["client"]["id"], "api_key": "incorrect-api-key-at-least-16"},
    )
    api.get("/v1/editor/policy", expected=403)
    editor.request("PATCH", f"/v1/editor/clients/{account['client']['id']}", expected=200, json={"trust": 0.7, "enabled": False})
    api.get("/v1/stats/me", expected=401)


# Проверяем, что повторы не добавляют голосов, а после публикации статистика авторов верна.
def test_publication_idempotency_trust_snapshot_and_statistics(editor, provision, policy):
    policy()
    first, alice = provision(trust=0.7)
    _, bob = provision(trust=0.5, role="outstaff")
    category, topic = unique(), unique()
    first_note = note(category, topic)
    receipt = submit(alice, first_note)
    assert receipt["published"] is False
    assert submit(alice, first_note, expected=200) == {**receipt, "status": "duplicate"}
    alice.post("/v1/notes", expected=409, json={**first_note, "body": "Changed source content."})
    assert submit(alice, note(category, topic))["published"] is False
    editor.request("PATCH", f"/v1/editor/clients/{first['client']['id']}", expected=200, json={"trust": 0.05, "enabled": True})
    assert submit(bob, first_note)["published"] is True
    story_path = f"/v1/stories/{receipt['story_id']}"
    published = bob.get(story_path).json()
    assert published["state"] == "published"
    assert [(item["rank"], item["trust"]) for item in published["contributors"]] == [(1, 0.7), (2, 0.5)]
    _, carol = provision(trust=1)
    submit(carol, first_note)
    assert carol.get(story_path).json() == published
    stats = alice.get("/v1/stats/me").json()
    assert stats["accepted"] == 2
    assert stats["published"] == 1
    assert stats["publication_share"] == 0.5


def test_event_time_decay(provision, policy):
    policy(threshold=0.9, half_life_seconds=3600)
    _, alice = provision(trust=0.8)
    _, bob = provision(trust=0.8)
    category, topic = unique(), unique()
    old = submit(alice, note(category, topic, seconds_ago=7200))
    story = alice.get(f"/v1/stories/{old['story_id']}").json()
    assert story["state"] == "collecting"
    assert 0.19 <= story["trust"] <= 0.20
    assert submit(bob, note(category, topic))["published"] is True


# Сообщения доступны после нового входа; подтверждение убирает только выбранное сообщение.
def test_offline_notifications_limit_and_individual_ack(public_api, provision, policy):
    policy(congratulations_count=1)
    owner, alice = provision(trust=0.7)
    _, bob = provision(trust=0.5)
    for _ in range(2):
        value = note()
        submit(alice, value)
        submit(bob, value)
    assert inbox(bob) == []
    reconnected = public_api.login(owner["client"]["id"], owner["api_key"])
    messages = inbox(reconnected)
    assert len(messages) == 2
    assert inbox(reconnected) == messages
    bob.post(f"/v1/messages/{messages[0]['id']}/ack", expected=404)
    reconnected.post(f"/v1/messages/{messages[0]['id']}/ack", expected=204)
    assert inbox(reconnected) == messages[1:]


def test_recover_client_key_after_lost_response(public_api, editor, provision):
    client_id = unique()
    client = dict(id=client_id, name="Lost response", role="outstaff", trust=0.4, enabled=True)
    # Discard the creation response, as if it never reached the editor.
    editor.post("/v1/editor/clients", expected=201, json=client)
    editor.post("/v1/editor/clients", expected=409, json=client)
    path = f"/v1/editor/clients/{client_id}/key"
    public_api.post(path, expected=401)
    _, reporter = provision()
    reporter.post(path, expected=403)
    editor.post(f"/v1/editor/clients/{unique()}/key", expected=404)

    abandoned_key = editor.post(path).json()["api_key"]
    replacement_key = editor.post(path).json()["api_key"]
    assert replacement_key != abandoned_key
    public_api.post("/v1/handshake", expected=401,
                    json={"client_id": client_id, "api_key": abandoned_key})
    session = public_api.post("/v1/handshake",
                              json={"client_id": client_id, "api_key": replacement_key}).json()
    assert session["client"] == client
    assert public_api.login(client_id, replacement_key).get("/v1/stats/me").json()["accepted"] == 0

    # A key reset must not reactivate a disabled account.
    editor.request("PATCH", f"/v1/editor/clients/{client_id}", expected=200,
                   json={"trust": 0.4, "enabled": False})
    disabled_key = editor.post(path).json()["api_key"]
    public_api.post("/v1/handshake", expected=401,
                    json={"client_id": client_id, "api_key": disabled_key})


def test_invalid_notes_do_not_change_statistics(provision):
    _, api = provision()
    api.post("/v1/notes", expected=422, json={**note(), "body": " "})
    assert api.get("/v1/stats/me").json()["accepted"] == 0
