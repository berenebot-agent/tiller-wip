# Security policy

Tiller Router is an alpha release. Please do not disclose a suspected
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
SLA for alpha releases.

## Scope and deployment notes

The Docker Compose deployment deliberately publishes `TILLER_PORT` on all host
interfaces for direct LAN access. Restrict it with the host firewall or a
private network when public/direct access is not intended. Keep the admin
interface private, use HTTPS at the edge, protect `./data`, and never commit
`.env` or provider credentials. Proxy-header trust must remain disabled unless
the direct proxy peer is restricted with `TILLER_TRUSTED_PROXY`.

For questions that are not security reports, please use the project's normal
public issue and discussion channels.
