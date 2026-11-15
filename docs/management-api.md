# Management API

Everything the Tiller dashboard does goes through a JSON REST API under
`/api/admin/*`. With a **management API token** you can drive that same API from
scripts, CI, or an agent — no browser session, cookie, or CSRF token.

This page is the narrative guide. The **authoritative route list** is generated
by each running instance from the routes it actually enforces, so it cannot
drift from this document:

| Where | What |
|---|---|
| `GET /api/admin` (or `/api`) | Rendered guide in a browser; plain markdown with `Accept: text/markdown` — hand this URL to an agent |
| `GET /api.json` | Discovery: name, API base, auth scheme, token prefix, scopes, capabilities, limits (and, on hosted, the rate and token limits) |
| `GET /api/openapi.json` | OpenAPI 3.1 description of every token-reachable route with its required scope (`x-required-scope`) |

All three are public and contain no secrets; they work on local and hosted
instances.

## 1. Create a token

Open **Settings → Management API → Create API token**, name it, choose an
expiry, and grant access per area. The token (`mt-tr-<selector>.<secret>`) is
shown **once**; only a hash is stored. You can rotate, disable, or delete it at
any time.

Access is per area with escalating levels — `read` ⊂ `write` ⊂ `delete`:

| Area | read | write | delete |
|---|---|---|---|
| Providers | inspect providers, models, OAuth status | create/edit providers, replace credentials, refresh discovery, connect OAuth, add manual models | delete providers/models, disconnect OAuth |
| Virtual models | inspect routes | create/edit groups and virtual models | delete groups and virtual models |
| Client keys | list keys, permissions, catalogue templates | create/edit/rotate keys and templates, edit permissions | delete keys and templates |
| Settings | read settings | change settings, send a test notification | — |
| Monitoring | usage, quota, activity (+CSV), cooldown, health | — | clear activity, clear cooldown |

### Hosted accounts

Hosted tokens follow the same model with extra guard rails:

- New tokens default to a 30-day expiry (you may choose no expiry).
- Tokens are limited to `read` and `write`; destructive deletes stay in the
  dashboard.
- Up to 10 tokens per account, sharing a budget of **60 requests per minute**
  (see [Rate limits](#rate-limits-hosted)).
- Suspending or deleting the account revokes tokens immediately. "Sign out
  everywhere" offers to revoke them too; a password reset leaves them in place.
- Provider base URLs must be public: addresses that resolve to private or
  internal networks are rejected, and some provider types are not offered.
- You receive an email when a token is created, if the instance has mail
  configured.

## 2. Authenticate

```bash
TILLER=https://your-tiller        # this instance's origin
TOKEN=mt-tr-...                   # the token you just created

curl -H "Authorization: Bearer $TOKEN" "$TILLER/api/admin/api-tokens/self"
```

A request that carries an `Authorization` header is decided by the token alone;
it never falls back to cookie authentication.

## 3. Quickstart

```bash
# List providers and their discovered models
curl -H "Authorization: Bearer $TOKEN" "$TILLER/api/admin/providers"

# Add a provider
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"my-provider","type":"generic-openai","base_url":"https://api.example.com/v1","credential":"sk-..."}' \
  "$TILLER/api/admin/providers"

# Point a virtual model at an upstream model
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"group_name":"main","name":"coding","targets":[{"provider_model_id":"<model-id>","enabled":true}]}' \
  "$TILLER/api/admin/virtual-models"

# Create a client key for an inference client (the secret is returned once)
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"hermes","type":"catalogue"}' \
  "$TILLER/api/admin/client-keys"
```

`{id}` in a path is a resource id returned by the matching list endpoint. Field
names above are illustrative; the dashboard's own requests are the reference for
payloads.

## Paging and limits

List endpoints accept `limit` (default 50, maximum 200), `offset` (capped at
10000) and, where supported, `search`. To read further than that, narrow with
`search` or use the CSV export routes. Admin request bodies are limited to
4 MiB. `/api.json` reports these as `page_size_default`, `page_size_max`,
`offset_max` and `admin_request_body_bytes`.

## Rate limits (hosted)

Hosted accounts share 60 management requests per minute across all their
tokens **and their console session**, in fixed one-minute windows that start at
the account's first request. The budget covers every console route, including
the token lifecycle routes and the account export, so an account cannot dodge it
by switching credential or screen. Every response on a budgeted route carries:

| Header | Meaning |
|---|---|
| `X-RateLimit-Limit` | requests allowed per window (60) |
| `X-RateLimit-Remaining` | requests left in this window |
| `X-RateLimit-Reset` | seconds until the window ends and the budget refills |

When the budget is spent the API answers `429` with `Retry-After` (the same
seconds-until-reset value) and a body such as:

```json
{"error":{"code":"rate_limited","message":"...","limit":60,"window_seconds":60,"retry_after":23}}
```

Refused requests are not counted and do not extend the wait. A local instance
has a single operator account, which draws on the same console budget;
failed-authentication attempts are throttled separately.

## Errors

Failures use `{"error":{"code":"...","message":"..."}}`:

| Status | Code | Meaning |
|---|---|---|
| 401 | `invalid_api_token` | missing, unknown, disabled, or malformed token |
| 401 | `token_expired` | the token's expiry has passed |
| 403 | `insufficient_scope` | the token's ceiling is below the route's tier; `required` names the grant (e.g. `providers:write`) |
| 429 | `authentication_rate_limited` | too many failed attempts; see `Retry-After` |
| 429 | `rate_limited` | hosted only: the account's request budget is spent; body has `limit`, `window_seconds`, `retry_after` |

## What tokens cannot do

Token lifecycle (list, create, edit, rotate, revoke) is dashboard-only, so a
leaked token cannot mint siblings or widen itself. Backup and account export,
identity operations (password, passkeys, email, account deletion, sessions),
onboarding, debug endpoints, and the live event stream are never token-reachable.

For embedding Tiller headless with an environment-managed token, see
[ADVANCED.md](ADVANCED.md#embedding-tiller). Security properties are described in
[SECURITY.md](../SECURITY.md).
