*** DRAFT FOR LEGAL REVIEW ***

This document is an engineering first draft prepared for legal review. It has
not been reviewed or approved by a qualified lawyer. It contains unresolved
[PLACEHOLDER] tokens. Do not publish it to end users or rely on it as legal
advice. Do not treat any part of it as final until a lawyer has signed off.

================================================================
SECURITY AND DATA HANDLING
================================================================

Operator:        [OPERATOR_LEGAL_NAME]
Effective:       [EFFECTIVE_DATE]
Security contact:[SECURITY_EMAIL]

This page summarises how Hosted Tiller ("the Service") protects data and how it
handles request content. It should be read with the Privacy Policy, the
Subprocessor List and the Terms of Service.

1. DATA-HANDLING MODEL

1.1 The Service is deliberately metadata-minimising. In hosted V1:

  (a) Account information (email, authentication records, plan, settings) is
      persisted.
  (b) Activity metadata (models, routing outcome, timing, status, token counts)
      is persisted for the applicable retention period.
  (c) Prompt and response content is processed transiently and is not persisted
      by Hosted Tiller. It is forwarded to the provider you configure, under
      that provider's terms.
  (d) Provider credentials are persisted encrypted.
  (e) Operational logs are metadata only, and do not contain prompts,
      responses, API keys or tokens.

1.2 Detailed body logging (which would store failed request bodies or provider
error bodies) is disabled in hosted V1. It is not available as a setting.

2. ENCRYPTION OF PROVIDER CREDENTIALS

2.1 Provider credentials - API keys, OAuth access/refresh/id tokens, OAuth
provider data and the notification authorization header - are recoverable
secrets that the Service must use to call your providers. They are encrypted at
rest using authenticated encryption (AES-256-GCM).

2.2 The master key is held outside the database, in a location the database dump
does not contain, and must be backed up separately. A copy of the database alone
does not reveal provider credentials.

2.3 Credentials are decrypted only as late as practical, only to make the
outbound provider call. They are never:
  (a) displayed back in plaintext after entry;
  (b) written to logs, error messages or crash output;
  (c) exposed through platform-admin interfaces; or
  (d) included in analytics or persisted to temporary files.

2.4 Key rotation is supported without requiring you to reconnect every provider.

3. HASHED SECRETS

3.1 Secrets that do not need to be recovered are stored only as hashes:
  (a) human-chosen passwords use a memory-hard password hash (Argon2id);
  (b) high-entropy machine tokens (client API keys, session tokens) use a slow,
      salted password hash (bcrypt) suited to their threat model.

3.2 Plaintext secrets are never stored, and are never re-displayed after
creation.

4. ACTIVITY: METADATA ONLY

4.1 Activity stores routing metadata, not content. It is account-scoped, so one
account cannot see another account's Activity.

4.2 Activity is pruned on the schedule described in the Privacy Policy. Security
and audit records are kept separate from Activity, with their own retention, so
pruning Activity does not erase security history.

5. TRANSPORT AND SESSION SECURITY

5.1 All client and control-plane traffic is served over HTTPS/TLS. Session
cookies are set with the Secure, HttpOnly and SameSite attributes and a narrow
host scope. Sessions are server-side, opaque and revocable, and are rotated on
sensitive authentication transitions.

5.2 State-changing control-plane requests require a CSRF token in addition to
the authenticated session.

6. OUTBOUND NETWORK AND SSRF CONTROLS

6.1 Hosted Tiller supports custom provider endpoints only through a shared safe
outbound transport that permits validated public HTTPS destinations.

6.2 The transport:
  (a) requires HTTPS and forbids embedded userinfo;
  (b) resolves the destination and validates every resolved address against a
      deny policy;
  (c) re-validates the actual address dialled, to defeat DNS-rebinding and
      time-of-check/time-of-use attacks;
  (d) re-validates every redirect under the same policy and bounds redirects;
  (e) applies bounded connect/read/header timeouts and bounded response sizes.

6.3 The deny policy blocks loopback, private, link-local, multicast, reserved
and unspecified address ranges, cloud-metadata destinations, and our own
internal infrastructure ranges.

6.4 Application-layer controls are backed by network egress controls, so
user-driven traffic cannot reach cloud metadata, internal admin services, private
service networks, or database endpoints except through the application's explicit
database path.

7. TENANT ISOLATION

7.1 The account is the tenancy boundary. Account authority is always derived
from an authenticated principal, never from a URL, body or header value supplied
by the client. Every tenant-owned read and write is account-scoped in the
persistence layer.

7.2 A client API key authorises exactly one account. A client cannot reach data
or models belonging to another account, even if it guesses or infers an
identifier.

8. OPERATIONAL LOGGING

8.1 Operational logs record metadata and opaque identifiers only. They do not
record prompt or response bodies, tool arguments, reasoning content, credentials
or API keys, and this is true at every log level.

9. INCIDENT RESPONSE

9.1 We maintain a privacy and data-breach runbook covering:
  (a) incident identification and containment;
  (b) determining affected data and accounts;
  (c) preserving evidence;
  (d) revoking credentials and sessions where necessary;
  (e) assessing notification requirements; and
  (f) regulator, customer and user communications and post-incident remediation.

9.2 If a data breach is likely to result in serious harm and meets the threshold
under Australia's Notifiable Data Breaches scheme, we will notify affected
individuals and the Office of the Australian Information Commissioner as
required. We will also meet applicable EU/UK breach obligations where those
regimes apply.

9.3 Report a suspected security issue to [SECURITY_EMAIL]. We aim to
acknowledge reports promptly and to act on credible reports of vulnerabilities.

10. PLATFORM ADMINISTRATION

10.1 Platform operators have access only to the minimum needed to operate the
Service. Platform administrators cannot reveal stored provider credentials in
plaintext, and there is no customer impersonation ("login as") capability in V1.

11. CHANGES

11.1 We may update this page from time to time. Material changes will be
communicated as described in the Terms of Service.

12. CONTACT

Operator:        [OPERATOR_LEGAL_NAME]
Security contact:[SECURITY_EMAIL]
Address:         [REGISTERED_ADDRESS]

================================================================
END OF DRAFT SECURITY AND DATA HANDLING
================================================================
