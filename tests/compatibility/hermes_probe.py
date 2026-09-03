import http.cookiejar
import json
import os
import pathlib
import subprocess
import tempfile
import urllib.request


BASE = os.environ.get("TILLER_COMPAT_BASE_URL", "http://127.0.0.1:18080")
MOCK_BASE = os.environ.get("TILLER_COMPAT_MOCK_BASE_URL", "http://127.0.0.1:18081/v1")
ADMIN_PASSWORD = os.environ["TILLER_COMPAT_ADMIN_PASSWORD"]

jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def admin(method, path, body=None, csrf=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(BASE + path, data=data, method=method)
    if body is not None:
        request.add_header("Content-Type", "application/json")
    if csrf:
        request.add_header("X-CSRF-Token", csrf)
    with opener.open(request, timeout=10) as response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else {}


_, session = admin("POST", "/api/admin/session", {"username": "admin", "password": ADMIN_PASSWORD})
csrf = session["csrf_token"]
_, provider = admin("POST", "/api/admin/providers", {"name": "hermes", "type": "generic-openai", "base_url": MOCK_BASE, "protocols": ["chat", "responses", "messages"]}, csrf)
_, models = admin("GET", f"/api/admin/providers/{provider['id']}/models")
_, group = admin("POST", "/api/admin/virtual-groups", {"name": "hermes-virtual"}, csrf)
_, virtual = admin("POST", "/api/admin/virtual-models", {"group_id": group["id"], "name": "coding", "target_provider_id": provider["id"], "target_model_id": models["data"][0]["id"]}, csrf)
_, key = admin("POST", "/api/admin/client-keys", {
    "name": "Hermes Single-key compatibility restart",
    "type": "single",
    "single_model_name": "main",
    "single_target_type": "virtual",
    "single_target_id": virtual["id"],
}, csrf)

with tempfile.TemporaryDirectory() as home:
    hermes_home = pathlib.Path(home, ".hermes")
    hermes_home.mkdir()
    pathlib.Path(hermes_home, "config.yaml").write_text(f'''providers:
  tiller-chat:
    api: {BASE}/v1
    key_env: TILLER_ROUTER_KEY
    transport: chat_completions
    default_model: main
  tiller-responses:
    api: {BASE}/v1
    key_env: TILLER_ROUTER_KEY
    transport: codex_responses
    default_model: main
  tiller-messages:
    api: {BASE}
    key_env: TILLER_ROUTER_KEY
    transport: anthropic_messages
    default_model: main
''')
    env = os.environ.copy()
    env["HOME"] = home
    env["TILLER_ROUTER_KEY"] = key["secret"]
    for provider_name in ("tiller-chat", "tiller-responses", "tiller-messages"):
        result = subprocess.run(
            ["hermes", "--oneshot", "Return exactly hello", "--provider", provider_name, "--model", "main", "--ignore-rules"],
            cwd=home,
            env=env,
            capture_output=True,
            text=True,
            timeout=90,
        )
        assert result.returncode == 0, f"{provider_name}: {result.stderr[-2000:]}"
        assert "hello" in result.stdout.lower(), f"{provider_name}: {result.stdout[-2000:]}"

print("Hermes Single-key chat_completions, codex_responses, and anthropic_messages probes passed")
