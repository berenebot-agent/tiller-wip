# Advanced

Advanced operation of Tiller Router: headless administration, embedding,
hosted deployment, and network recovery. Installation and everyday usage are
covered in the [README](../README.md).

## Management API

The web interface is a pure client of a JSON REST API under `/api/admin/*`, and
you can drive it from scripts and agents with a **management API token**
(Settings → Management API). Tokens, scopes, the hosted limits, errors and a
curl quickstart are documented in **[management-api.md](management-api.md)**.

Each running instance also documents itself: `/api/admin` (rendered guide, or
markdown with `Accept: text/markdown`), `/api.json` (discovery) and
`/api/openapi.json` (OpenAPI 3.1), all generated from the routes the instance
enforces.

## Embedding Tiller

Because the control plane is a plain JSON API, Tiller can run headless inside
another application as its LLM gateway and protocol translator, with the host
owning the front end. Set `TILLER_ADMIN_API_KEY` (or `TILLER_ADMIN_API_KEY_FILE`)
to a management token and the router seeds a single full-power token named
`boot-admin` at boot — the host provisions providers, models, routes, and client
keys entirely over the API, with no browser:

```bash
# 1. Create a management token in any Tiller's Settings → Management API,
#    or generate a fresh instance's seed by copying it from there once.
# 2. Boot the embedded instance with that token:
TILLER_ADMIN_API_KEY=mt-tr-... docker compose up -d
# 3. Point your host application's SDK at the instance:
curl -H "Authorization: Bearer mt-tr-..." http://tiller:8080/api/admin/providers
```

Notes for embedders:

- The seed is **full power** (every area at the delete ceiling) and never
  logged. It is environment-managed and cannot be edited, rotated, or revoked
  in the browser/token lifecycle API; bulk revocation excludes it. Rotate it
  by changing the configured key and restarting — the fixed
  identity replaces the old credential and invalidates it immediately.
- Permanently revoke it by removing both `TILLER_ADMIN_API_KEY` and
  `TILLER_ADMIN_API_KEY_FILE` and restarting; startup removes its stored row.
  The key file takes precedence when configured.
- A seed key counts as "configured": the instance does not report itself as
  needing first-run browser setup.
- Reach the management API over the bearer token; the token-lifecycle endpoints
  themselves remain browser/cookie-only, so the seed is the one credential the
  host manages out of band. Bind the instance to a private network/loopback and
  let the host application own all external access.
- Activity still records by default; turn it off per client key (`logging_enabled`)
  or account-wide (`default_logging_enabled`) if the host wants no history.
- Failed request and provider error bodies are stored only when detailed error
  logging is enabled. Per client key, `log_error_bodies` is `inherit` (follow
  the account setting), `always` or `never`; hosted mode refuses `always`.

```bash
# Create a token in Settings → Management API, then:
curl -H "Authorization: Bearer mt-tr-..." \
  https://your-router.example.com/api/admin/providers

# Create a virtual model (the canonical id becomes <group>/<name>)
curl -H "Authorization: Bearer mt-tr-..." -H "Content-Type: application/json" \
  -d '{"group_name":"main","name":"coding","targets":[{"provider_model_id":"...","enabled":true}]}' \
  https://your-router.example.com/api/admin/virtual-models
```

## Hosted Compose networking

The main `docker-compose.yml` remains the simple self-hosted appliance: it
keeps direct `TILLER_PORT` publishing and does not require a reverse proxy.
Hosted deployments use the separate `docker-compose.hosted.yml` override:

```bash
TILLER_PUBLIC_URL=https://app.example.com TILLER_TRUSTED_PROXY=172.20.0.2/32 \
docker compose -f docker-compose.yml -f docker-compose.hosted.yml up -d --build
```

Set the required platform admin credentials in `.env` as well. Choose the
trusted proxy IP/CIDR from the router's ingress network; don't trust the entire
network unless every host on it is a controlled reverse proxy.

The hosted override removes direct host-port publishing and attaches Tiller to
a managed `tiller-router-egress` network containing only Tiller, plus an
ingress network for the reverse proxy. It does not use the implicit Compose
default network.

If the reverse proxy runs in a different Compose project, point the hosted
override at its existing Docker network:

```dotenv
TILLER_INGRESS_NETWORK=proxy_network
TILLER_INGRESS_NETWORK_EXTERNAL=true
```

The hosted deployment requires `TILLER_PUBLIC_URL` and
`TILLER_TRUSTED_PROXY`, set to the direct reverse proxy's IP/CIDR so client IP
rate limits and audit records use the real client address. It still uses the
application-level `SafeTransport` for public HTTPS
destination validation and SSRF protection. Docker network isolation is only
defense in depth; it is not a substitute for the application policy or an
optional VPS firewall hardening rule.

## Front-door request limits

Tiller meters a logged-in account's console traffic (dashboard and API tokens
share one budget) and throttles repeated failed password confirmations. It does
**not** meter per source IP: behind a reverse proxy the app only knows the
address the proxy forwards, so a per-IP allowance belongs at the proxy, where
the real peer is known and a flood can be rejected before it reaches Go.

nginx:

```nginx
# http block
limit_req_zone $binary_remote_addr zone=tiller_general:10m rate=20r/s;
limit_req_zone $binary_remote_addr zone=tiller_auth:10m    rate=2r/s;

# server block
limit_req zone=tiller_general burst=40 nodelay;
location /api/auth/ {
    limit_req zone=tiller_auth burst=5 nodelay;
    proxy_pass http://tiller-router:8080;
}
```

Caddy:

```caddy
app.example.com {
    # caddy-ratelimit plugin (github.com/mholt/caddy-ratelimit)
    rate_limit {
        zone general {
            key {remote_host}
            events 20
            window 1s
        }
    }
    reverse_proxy tiller-router:8080
}
```

The app's own budgets are intentionally generous so ordinary use never touches
them; the proxy limit is the layer that keeps one noisy source from consuming
the connection pool. Two proxy settings are required for the dashboard to work
regardless of the limits above: disable response buffering for
`/api/admin/live` (nginx `proxy_buffering off;`), and keep the proxy's read
timeout above the stream's 5s heartbeat.

## Hosted Google sign-in and Turnstile

Configure customer sign-in from **`/platform` → Signup and mail** after the
hosted service is running. These integrations are off until enabled there, and
their secrets are encrypted at rest with the platform settings.

For Google sign-in, create an OAuth client for a web application in Google
Cloud. Add the redirect URI displayed beside the client ID field in the
platform dashboard; it is the exact URL
`https://<your-hostname>/api/auth/google/callback`. Enter the OAuth client ID
and secret, then enable Google sign-in. Google supplies the verified email and
stable subject identifier.

A Google email that already belongs to a Tiller account is linked
automatically **only when Google is authoritative for that address** — a
`gmail.com`/`googlemail.com` address, or a Google Workspace account (a verified
email carrying Google's `hd` claim). For any other address, Google's
`email_verified` does not prove current ownership of the mailbox (the address
may since have changed hands), so Tiller does not link it silently: sign in to
the existing account with your password, and Tiller offers to link Google on
the spot. You can also link at any time from **Settings → Account**.

For bot protection, create a Cloudflare Turnstile widget and allow your hosted
hostname in its widget settings. Enter its site key and secret key in the
platform dashboard, then enable Turnstile. The site key is public; the secret
is kept server-side. Turnstile protects signup, verification resend, password
reset requests, and the start of Google sign-in. Password login keeps its
existing rate limits. Turnstile works without routing the site through
Cloudflare. See the [Google web-server OAuth guide](https://developers.google.com/identity/protocols/oauth2/web-server)
and [Cloudflare Turnstile setup guide](https://developers.cloudflare.com/turnstile/get-started/).

The Google OAuth client secret and Turnstile secret key are encrypted at rest.
The browser receives neither secret. If you rotate a secret, enter the new
value and save; leave it blank to keep the stored value. Use the explicit
“Clear saved … secret” checkbox to remove one.

## DNS recovery after restarts

Tiller uses system/Docker DNS first. If those servers fail (for example, Docker
restores a container before DHCP supplies upstream DNS), it tries Cloudflare
(`1.1.1.1`, then `1.0.0.1`), followed by Google (`8.8.8.8`, then `8.8.4.4`).
This is built into the application and works with either deployment option;
no host-specific DNS override, privileged access, or rebuild is needed to recover
from a later DNS failure.

Successful replies, including private/LAN addresses, are retained. Valid
NXDOMAIN (name does not exist) and NODATA (no record of the requested type)
replies do **not** trigger public fallback. Hosts-file entries and the native
resolver's search-domain and UDP/TCP handling are retained. Each lookup starts
with supplied DNS again, so recovery does not permanently switch DNS servers.
Hosted mode still validates every destination address before connecting.

Set `TILLER_DNS_FALLBACK_SERVERS` to a comma-separated list of IP addresses
(optionally with ports; IPv6 with a port uses `[address]:port`) to change the
fallback order. Empty/unset uses the defaults above. Set it to `off` to use only
supplied DNS, for example on networks where DNS queries must remain internal.
When fallback is enabled, failed queries can be sent to the fallback servers;
this includes private names/search-domain queries if their supplied DNS fails.
Only DNS failures trigger failover; HTTP errors and provider connection failures
do not select another DNS server or retry a provider request.
