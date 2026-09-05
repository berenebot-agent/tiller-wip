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

**Provider credentials are not encrypted at rest.** They are stored in
recoverable form in the SQLite database (`./data`) so Tiller can authenticate
upstream requests, and credential encryption at rest is a future-roadmap
consideration. Take care with where you store the persistent database and any
backups of it — treat `./data` and its backups as secrets, since anyone who can
read the database file can recover your provider keys.

Migration 024 clears request and provider response body columns from the live
database; it is not secure erasure. SQLite pages, WAL files, snapshots, and old
backups may still contain historic sensitive data, so they must continue to be
protected as sensitive material.

**Detailed error logging (opt-in).** Activity is metadata-only by default. If the
administrator enables the Detailed Error Logging setting, failed request bodies
and provider error bodies are stored (bounded to 1 MiB). Activity exports
containing those records must be treated as sensitive. The setting defaults to
disabled and is presented with a warning in the admin UI.

For questions that are not security reports, please use the project's normal
public issue and discussion channels.
