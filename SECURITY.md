# Security policy

Tiller Router is a beta release. Please do not disclose a suspected
vulnerability in a public issue, discussion, or pull request.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository: open the
repository's **Security** tab, choose **Report a vulnerability**, and submit a
private security advisory. This is the supported reporting channel; no public
email address is assumed or required.

Include enough information to reproduce the issue safely, such as the affected
version or commit, deployment shape, request path (without credentials or
personal data), impact, and a minimal reproduction. Please redact provider
credentials, client API keys, session cookies, prompts, and responses.

We will acknowledge reports when practical and coordinate a fix, disclosure,
and credit with the reporter. There is no guaranteed response or remediation
SLA for beta releases.

## Scope and deployment notes

The Docker Compose deployment deliberately publishes `TILLER_PORT` on all host
interfaces for direct LAN access. Restrict it with the host firewall or a
private network when public/direct access is not intended. Keep the admin
interface private, use HTTPS at the edge, protect `./data`, and never commit
`.env` or provider credentials. Proxy-header trust must remain disabled unless
the direct proxy peer is restricted with `TILLER_TRUSTED_PROXY`.

**Recoverable provider credentials are encrypted at rest (always on).**
Provider API credentials, OAuth access/refresh/id tokens and `provider_data`, and
the notification auth header are sealed with AES-256-GCM (versioned `enc:v1:`
format, unique nonce per value, associated data binding the account/record/field)
at the `internal/store` boundary. The master key is kept **outside the database**:
set `TILLER_MASTER_KEY` (base64 of 32 random bytes) or `TILLER_MASTER_KEY_FILE`
(takes precedence), or let Tiller generate one at `<data dir>/master.key` (0600)
on first start. **Back the master key up separately from `./data`** — without it,
existing encrypted credentials cannot be recovered. A database dump therefore
does not reveal provider credentials; a full `./data` archive does include the
generated key and must be protected as a secret. Rotate the key with
`tiller-router rotate-master-key` (service stopped; new key via
`TILLER_MASTER_KEY_NEW` or `TILLER_MASTER_KEY_NEW_FILE`). If encrypted values
exist but the key is missing or wrong, Tiller starts in a **locked** state:
credential-bearing providers are unavailable and no plaintext credential can be
written until the correct key is restored.

Migration 024 clears request and provider response body columns from the live
database; it is not secure erasure. SQLite pages, WAL files, snapshots, and old
backups may still contain historic sensitive data, so they must continue to be
protected as sensitive material.

**Secret hashing is entropy-tiered.** Non-recoverable secrets (client API keys,
admin session tokens, the admin credential fingerprint) are hash-only at rest;
plaintext is never stored. The admin credential fingerprint is human-chosen and
low-entropy, so it is protected with a memory-hard KDF (**argon2id, 64 MiB**).
Client API keys and admin session tokens are 256-bit uniformly random, where
offline brute force is infeasible regardless of hash speed; those use
**bcrypt (cost ≥ 10)**, whose fixed ~4 KiB working set avoids the ~64 MiB
per-verify memory cost that dominated the process footprint under concurrent
authentication. Existing argon2id values keep verifying (algorithm is
dispatched from the encoding) and are upgraded to bcrypt lazily on the next
successful authentication, so the tiering change needs no forced migration or
downtime.

**Auth caches and revocation.** Verified client keys and admin sessions are
cached in memory and the cache entry renews on use; the TTLs are configurable
(client key 15m, session 5m by default, clamped to ≤24h). Revocation does not
wait for the TTL: any client-key or session mutation invalidates the affected
cache entries immediately, and credential changes revoke all sessions. The TTL
is therefore a *cross-instance* revocation bound — with a single process it has
no security effect, and the sliding cache exists to keep hash-verification cost
proportional to the rate of distinct/expired keys rather than request volume.
Both caches are periodically swept and capped so they cannot grow unbounded.

**Detailed error logging (opt-in).** Activity is metadata-only by default. If the
administrator enables the Detailed Error Logging setting, failed request bodies
and provider error bodies are stored (bounded to 1 MiB). Activity exports
containing those records must be treated as sensitive. The setting defaults to
disabled and is presented with a warning in the admin UI. This is the documented
exception to the AGENTS.md no-content guardrail, which is scoped to default
logging.

For questions that are not security reports, please use the project's normal
public issue and discussion channels.
