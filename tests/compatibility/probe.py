import http.cookiejar
import json
import os
import pathlib
import subprocess
import tempfile
import urllib.request

from anthropic import Anthropic
from openai import OpenAI


BASE = os.environ.get("TILLER_COMPAT_BASE_URL", "http://127.0.0.1:18080")
MOCK_BASE = os.environ.get("TILLER_COMPAT_MOCK_BASE_URL", "http://127.0.0.1:18081/v1")
ADMIN_USER = os.environ.get("TILLER_COMPAT_ADMIN_USERNAME", "admin")
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


_, session = admin("POST", "/api/admin/session", {"username": ADMIN_USER, "password": ADMIN_PASSWORD})
csrf = session["csrf_token"]
_, provider = admin("POST", "/api/admin/providers", {"name": "compat", "type": "generic-openai", "base_url": MOCK_BASE, "protocols": ["chat", "responses", "messages"]}, csrf)
provider_id = provider["id"]
_, model_page = admin("GET", f"/api/admin/providers/{provider_id}/models")
model_id = model_page["data"][0]["id"]
_, group = admin("POST", "/api/admin/virtual-groups", {"name": "virtual"}, csrf)
_, virtual = admin("POST", "/api/admin/virtual-models", {"group_id": group["id"], "name": "coding", "target_provider_id": provider_id, "target_model_id": model_id}, csrf)
_, client_key = admin("POST", "/api/admin/client-keys", {"name": "SDK compatibility"}, csrf)
secret = client_key["secret"]
admin("PUT", f"/api/admin/client-keys/{client_key['id']}/permissions", {"defaults": [], "permissions": [{"kind": "virtual", "model_id": virtual["id"], "enabled": True}]}, csrf)

openai = OpenAI(base_url=BASE + "/v1", api_key=secret)
models = openai.models.list()
assert [model.id for model in models.data] == ["virtual/coding"]
chat = openai.chat.completions.create(model="virtual/coding", messages=[{"role": "user", "content": "hello"}])
assert chat.model == "virtual/coding" and chat.choices[0].message.content == "hello"
chat_text = "".join(chunk.choices[0].delta.content or "" for chunk in openai.chat.completions.create(model="virtual/coding", messages=[{"role": "user", "content": "hello"}], stream=True) if chunk.choices)
assert chat_text == "hello"
response = openai.responses.create(model="virtual/coding", input="hello")
assert response.model == "virtual/coding" and response.output_text == "hello"
response_events = list(openai.responses.create(model="virtual/coding", input="hello", stream=True))
assert any(event.type == "response.output_text.delta" and event.delta == "hello" for event in response_events)
assert response_events[-1].type == "response.completed"

anthropic = Anthropic(base_url=BASE, api_key=secret)
message = anthropic.messages.create(model="virtual/coding", max_tokens=32, messages=[{"role": "user", "content": "hello"}])
assert message.model == "virtual/coding" and message.content[0].text == "hello"
with anthropic.messages.stream(model="virtual/coding", max_tokens=32, messages=[{"role": "user", "content": "hello"}]) as stream:
    assert "".join(stream.text_stream) == "hello"

with tempfile.TemporaryDirectory() as codex_home:
    pathlib.Path(codex_home, "config.toml").write_text(f'''model = "virtual/coding"
model_provider = "tiller"
approval_policy = "never"
sandbox_mode = "read-only"

[model_providers.tiller]
name = "Tiller Router"
base_url = "{BASE}/v1"
env_key = "TILLER_ROUTER_KEY"
wire_api = "responses"
''')
    codex_env = os.environ.copy()
    codex_env["CODEX_HOME"] = codex_home
    codex_env["TILLER_ROUTER_KEY"] = secret
    result = subprocess.run(
        ["codex", "exec", "--skip-git-repo-check", "Return exactly hello"],
        cwd=codex_home,
        env=codex_env,
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert result.returncode == 0, result.stderr[-2000:]
    assert "hello" in result.stdout.lower(), result.stdout[-2000:]

with tempfile.TemporaryDirectory() as opencode_home:
    pathlib.Path(opencode_home, "opencode.json").write_text(json.dumps({
        "$schema": "https://opencode.ai/config.json",
        "provider": {"tiller": {
            "npm": "@ai-sdk/openai-compatible",
            "name": "Tiller Router",
            "options": {"baseURL": BASE + "/v1", "apiKey": secret},
            "models": {"virtual/coding": {"name": "Virtual / Coding"}},
        }},
        "model": "tiller/virtual/coding",
    }))
    opencode_env = os.environ.copy()
    opencode_env["HOME"] = opencode_home
    result = subprocess.run(
        ["opencode", "run", "Return exactly hello"],
        cwd=opencode_home,
        env=opencode_env,
        capture_output=True,
        text=True,
        timeout=90,
    )
    assert result.returncode == 0, result.stderr[-2000:]
    assert "hello" in result.stdout.lower(), result.stdout[-2000:]

with tempfile.TemporaryDirectory() as claude_home:
    claude_env = os.environ.copy()
    claude_env.update({
        "HOME": claude_home,
        "ANTHROPIC_BASE_URL": BASE,
        "ANTHROPIC_AUTH_TOKEN": secret,
        "ANTHROPIC_MODEL": "virtual/coding",
    })
    result = subprocess.run(
        ["claude", "-p", "Return exactly hello", "--model", "virtual/coding", "--output-format", "text"],
        cwd=claude_home,
        env=claude_env,
        capture_output=True,
        text=True,
        timeout=90,
    )
    assert result.returncode == 0, result.stderr[-2000:]
    assert "hello" in result.stdout.lower(), result.stdout[-2000:]

print("official SDK, Codex CLI, OpenCode, and Claude Code compatibility probes passed")
