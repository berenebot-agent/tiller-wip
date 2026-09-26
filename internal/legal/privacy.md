*** DRAFT FOR LEGAL REVIEW ***

This document is an engineering first draft prepared for legal review. It has
not been reviewed or approved by a qualified lawyer. It contains unresolved
[PLACEHOLDER] tokens and placeholder vendor names that must be replaced with the
actual, contracted subprocessors before publication. Do not publish it to end
users or rely on it as legal advice. Do not treat any part of it as final until
a lawyer has signed off.

================================================================
PRIVACY POLICY
================================================================

Operator:    [OPERATOR_LEGAL_NAME]
ABN:         [ABN]
Address:     [REGISTERED_ADDRESS]
Privacy contact: [PRIVACY_EMAIL]
Security contact:[SECURITY_EMAIL]
Effective:   [EFFECTIVE_DATE]

1. WHO WE ARE

1.1 This Privacy Policy explains how [OPERATOR_LEGAL_NAME] ("we", "us", "our")
handles personal information collected through the hosted service operated under
the name "tiller-router" ("Hosted Tiller", "the Service").

1.2 We are an Australian-operated service, and our users may be located
anywhere in the world. Our infrastructure and service providers may process
personal information outside Australia, including in the United States. See
section 8.

1.3 We aim to comply with the Australian Privacy Principles (APPs) in the
Privacy Act 1988 (Cth). Where the European Economic Area, the United Kingdom or
another jurisdiction's data-protection law applies to you, additional rights may
also apply to you. This Policy is written to be read alongside any such rights.

1.4 This Policy should be read with our Terms of Service.

2. THE DATA WE HANDLE, AND HOW WE HANDLE IT

2.1 The Service is deliberately metadata-minimising. The categories below are
the complete V1 data model:

  (a) Account information - your email address, authentication records (such as
      a password hash and session records), your plan, and your account
      settings. This information is persisted by Hosted Tiller.

  (b) Activity metadata - request metadata such as the model requested, the
      resolved target, protocol, whether the request was streamed, the HTTP
      outcome, latency, token counts where available, fallback attempts and
      outcomes, provider request IDs where safe, Tiller request IDs, and
      timestamps. This information is persisted for the applicable retention
      period (see section 9).

  (c) Prompt and response content - the request content you send and the
      response content returned by the provider. This content is processed
      transiently by Hosted Tiller in order to route it to the provider you
      configure. Hosted V1 does NOT persist prompt or response bodies.

  (d) Provider credentials - the API keys, OAuth tokens and related secrets you
      connect so Hosted Tiller can call your providers. These are persisted
      encrypted (see section 7).

  (e) Operational logs - metadata only. Operational logs do not contain prompts,
      responses, API keys or tokens.

  (f) Sign-in integration data - if you choose Google sign-in, Google returns
      your verified email address and stable account subject to Hosted Tiller.
      Hosted Tiller stores those values to authenticate and link your account.
      If the operator enables Cloudflare Turnstile, Cloudflare processes the
      security token and browser signals used to reduce automated abuse.

2.2 We do not ask you to provide, and the Service is not designed to require,
"special category" or sensitive information. You are responsible for what you
choose to send through the Service. Because prompt content is processed
transiently and not persisted, it is not part of our stored data model, but we
necessarily receive it while routing it.

3. HOW WE COLLECT PERSONAL INFORMATION

3.1 We collect personal information:
  (a) directly from you when you create an account, verify your email,
      authenticate, configure the Service, connect a provider, or contact us;
  (b) automatically when you use the Service, in the form of Activity metadata
      and operational logs; and
  (c) from service providers who process data on our behalf (see section 8).

3.2 If you choose Google sign-in, Google processes the authorization request
and returns your verified email address and stable account subject. We request
only the OpenID Connect identity and email scopes. If Turnstile is enabled,
Cloudflare receives the challenge request and token validation request,
including browser and network information needed to assess abuse.

3.3 We use only strictly necessary authentication and security cookies. We do
not use non-essential tracking, advertising or analytics cookies. If that
changes, we will update this Policy and, where required, obtain consent before
deploying such technologies.

4. WHY WE USE PERSONAL INFORMATION

4.1 We use personal information to:
  (a) create and administer your account and authenticate you;
  (b) operate, secure and provide the Service, including routing your requests
      to the providers you configure;
  (c) maintain Activity metadata so you can see routing behaviour and usage;
  (d) enforce plan quotas and limits, and prevent abuse, fraud and security
      incidents;
  (e) provide support and respond to your enquiries;
  (f) comply with law and enforce our Terms of Service; and
  (g) improve the reliability and security of the Service.

4.2 We do not sell your personal information. We do not use your prompt or
response content to train models.

5. ACCOUNT AND AUTHENTICATION DATA

5.1 We hold your email address, an email-normalised form of it, a password
hash (where password login is used), email-verification state, account status,
and session records. Passwords are stored only as a memory-hard hash and are
never stored or logged in plaintext.

5.2 Session records are stored server-side and referenced by an opaque token.
Sessions expire and are revoked on password reset, account compromise or
account deletion, as described in the service documentation.

5.3 Google sign-in is optional alongside email and password. We store Google's
stable subject identifier and verified email so later sign-ins use the same
identity. We do not connect a Google identity to an existing account based only
on matching email addresses. OAuth tokens are used to complete sign-in and are
not retained. Google-first accounts can add a password after confirming their
identity through Google.

6. ACTIVITY METADATA

6.1 Activity metadata is the routing and outcome information listed in
section 2.1(b). It is account-scoped and visible to you in the Service for your
retention window. It does not contain prompt or response bodies.

6.2 The retention period depends on your plan. The free plan retains Activity
metadata for a limited window (currently 7 days); other plans may retain it for
longer. Retention is enforced (pruned) on a schedule, and any shortened
retention is applied prospectively without rewriting records already stored.

7. PROVIDER CREDENTIALS

7.1 Provider credentials are recoverable secrets that the Service must be able
to use to call your providers. They are encrypted at rest using authenticated
encryption (AES-256-GCM), with the master key held outside the database. They
are decrypted only as needed to make the outbound provider call, and are never
displayed back in plaintext after you enter them, never logged, and never
included in platform-admin views.

7.2 The master key must be backed up separately from the database. A copy of the
database alone does not reveal provider credentials, but if the master key is
lost the Service cannot recover encrypted credentials.

8. SUBPROCESSORS AND OVERSEAS PROCESSING AND DISCLOSURE

8.1 We use a small number of third-party service providers ("subprocessors") to
operate the Service. We are an Australian-operated service and users may be
located anywhere in the world. Depending on the subprocessor and your location,
personal information may be processed or disclosed outside Australia, including
in the United States. We disclose those likely locations rather than making a
vague "anywhere" statement.

8.2 The current subprocessors, with purpose, data categories, whether data is
persisted or transient, country of processing, and transfer mechanism, are:

  Purpose                | Vendor (placeholder) | Data categories handled                     | Persisted / transient        | Country of processing | Transfer mechanism
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Cloud / compute host   | [CLOUD_VENDOR]       | Account information; Activity metadata;     | Persisted (encrypted at rest)| Australia and/or the  | Adequacy / SCC / DPF
                         |                      | encrypted provider credentials; operational |                              | United States         | as applicable
                         |                      | logs (metadata only); transient request &  | Transient for prompt/        |                       |
                         |                      | response content                            | response content             |                       |
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Transactional email    | [EMAIL_VENDOR]       | Email address; account event notifications; | Persisted by the vendor      | United States and/or  | SCC / DPF / UK
  (provider TBD: Resend, | (e.g. Resend, Brevo, | verification / password-reset tokens        | for delivery; tokens         | the vendor's region   | Extension as applicable
  Brevo or self-managed  | or SMTP operator)    |                                             | short-lived                  |                       |
  SMTP)                  |                      |                                             |                              |                       |
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Google sign-in         | Google               | OAuth authorization; verified email and     | Transient OAuth response;    | United States and/or  | To be confirmed
                         |                      | stable Google subject identifier            | Tiller stores subject/email  | Google's regions      | before launch
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Bot protection         | Cloudflare Turnstile | Challenge token; browser and network signals| Transient challenge and      | Global edge network   | To be confirmed
  (optional)             |                      | used for abuse prevention                   | validation data              |                       | before launch
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  DNS / TLS / edge       | [EDGE_VENDOR]        | Request metadata at the network edge;       | Transient (metadata only)    | Global edge network   | Adequacy / SCC /
  protection             |                      | connection metadata; TLS termination        |                              | (incl. Australia and  | DPF as applicable
                         |                      |                                             |                              | the United States)    |
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Error monitoring       | [MONITORING_VENDOR]  | Error and performance metadata (no prompt    | Persisted by the vendor      | United States         | SCC / DPF as
  (if enabled)           |                      | or response bodies; no credentials)         | for a limited window         |                       | applicable
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
  Billing (future, once  | [BILLING_VENDOR]     | Billing contact; payment metadata; plan      | Persisted by the vendor      | To be determined      | To be assessed
  paid plans launch)     |                      | status                                      |                              |                       | before enablement
  -----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------

8.3 Where personal information is transferred from the European Economic Area
to a country without an adequacy decision, we rely on Standard Contractual
Clauses with the relevant subprocessor (or another lawful Chapter V mechanism).
Where a US recipient is eligible under the EU-US Data Privacy Framework, we may
rely on that framework, and on the UK Extension to the EU-US Data Privacy
Framework for UK transfers where applicable. Where the UK IDTA/Addendum or
another UK safeguard is required, we put it in place with the relevant
subprocessor. Transfer positions are reassessed when a subprocessor or its
processing location changes.

8.4 We do not disclose personal information to any other third party except:
(a) with your direction (for example, routing your request to the provider you
configure); (b) to the subprocessors listed above; (c) where required by law or
to respond to lawful requests; or (d) to protect the rights, safety and security
of the Service, our users or others.

8.5 This list is reviewed before any new subprocessor that receives user data is
added. Adding such a subprocessor requires an update to this Policy and, where
appropriate, notice to users.

8.6 When you configure a provider, request content is sent to that provider
under that provider's own privacy terms. The provider is not our subprocessor;
it is a third party selected and controlled by you, and its handling of the
content is governed by your agreement with it.

9. RETENTION

9.1 We retain personal information only as long as necessary for the purposes
described in this Policy:

  (a) account information - while your account is active, and as needed to
      comply with law or resolve disputes;
  (b) Activity metadata - for the applicable plan retention window;
  (c) provider credentials - while connected, and until you remove them or
      delete your account;
  (d) operational logs - for a short operational period;
  (e) security and audit records - for a defined audit-retention period,
      which is kept separate from Activity retention so security history is not
      erased by Activity pruning.

9.2 When you delete your account, we delete or de-identify your account data
in accordance with this Policy. Some records may be retained where required by
law or for legitimate security, audit or dispute-resolution purposes. Residual
copies may remain in backups for a limited period, after which they expire
through the normal backup cycle. We do not promise instant physical erasure from
backups.

10. SECURITY

10.1 We use technical and organisational measures to protect personal
information.

10.2 Provider credentials - API keys, OAuth access/refresh/id tokens, OAuth
provider data and the notification authorization header - are encrypted at rest
using authenticated encryption (AES-256-GCM). The master key is held outside the
database in a location the database dump does not contain. Credentials are
decrypted only as late as practical, only to make the outbound provider call,
and are never displayed back in plaintext after entry, written to logs, error
messages or crash output, exposed through platform-admin interfaces, or included
in analytics or persisted to temporary files. Key rotation is supported without
requiring you to reconnect every provider.

10.3 Secrets that do not need to be recovered are stored only as hashes:
human-chosen passwords use a memory-hard password hash (Argon2id); high-entropy
machine tokens (client API keys, session tokens) use a slow, salted password
hash (bcrypt) suited to their threat model. Plaintext secrets are never stored
and are never re-displayed after creation.

10.4 All client and control-plane traffic is served over HTTPS/TLS. Session
cookies are set with the Secure, HttpOnly and SameSite attributes and a narrow
host scope. Sessions are server-side, opaque and revocable, and are rotated on
sensitive authentication transitions. State-changing control-plane requests
require a CSRF token in addition to the authenticated session.

10.5 Custom provider endpoints are supported only through a shared safe outbound
transport that permits validated public HTTPS destinations. The transport
requires HTTPS and forbids embedded userinfo; resolves the destination and
validates every resolved address against a deny policy; re-validates the actual
address dialled, to defeat DNS-rebinding and time-of-check/time-of-use attacks;
re-validates every redirect under the same policy and bounds redirects; and
applies bounded connect/read/header timeouts and bounded response sizes. The
deny policy blocks loopback, private, link-local, multicast, reserved and
unspecified address ranges, cloud-metadata destinations, and our own internal
infrastructure ranges. Application-layer controls are backed by network egress
controls.

10.6 The account is the tenancy boundary. Account authority is always derived
from an authenticated principal, never from a URL, body or header value supplied
by the client. Every tenant-owned read and write is account-scoped in the
persistence layer. A client API key authorises exactly one account, and a client
cannot reach data or models belonging to another account, even if it guesses or
infers an identifier.

10.7 Operational logs record metadata and opaque identifiers only. They do not
record prompt or response bodies, tool arguments, reasoning content, credentials
or API keys, and this is true at every log level. Detailed body logging (which
would store failed request bodies or provider error bodies) is disabled in
hosted V1 and is not available as a setting.

10.8 Platform operators have access only to the minimum needed to operate the
Service. Platform administrators cannot reveal stored provider credentials in
plaintext, and there is no customer impersonation ("login as") capability in V1.

10.9 We maintain a privacy and data-breach runbook covering incident
identification and containment; determining affected data and accounts;
preserving evidence; revoking credentials and sessions where necessary;
assessing notification requirements; and regulator, customer and user
communications and post-incident remediation.

10.10 No system is perfectly secure. If a data breach occurs that is likely to
result in serious harm and meets the threshold under the Notifiable Data
Breaches scheme, we will notify affected individuals and the Office of the
Australian Information Commissioner as required by law. We will also meet
applicable EU/UK breach obligations where those regimes apply.

10.11 Report a suspected security issue to [SECURITY_EMAIL]. We aim to
acknowledge reports promptly and to act on credible reports of vulnerabilities.

11. ACCESS, CORRECTION AND DELETION

11.1 You can access and update much of your account information through the
Service. To request access to, correction of, or deletion of personal
information we hold about you, contact us at [PRIVACY_EMAIL].

11.2 You can delete your account through the account settings. On deletion we
will take the steps described in section 9.2.

11.3 We will respond to access and correction requests within a reasonable
period and as required by applicable law. We may need to verify your identity
before acting on a request.

12. COMPLAINTS

12.1 If you believe we have handled your personal information in breach of
applicable privacy law, contact us at [PRIVACY_EMAIL] and we will investigate.

12.2 If you are not satisfied with our response, you may complain to the Office
of the Australian Information Commissioner (OAIC) at https://www.oaic.gov.au. If
you are in the EEA or the UK, you may also have the right to complain to your
local supervisory authority or the UK Information Commissioner's Office.

13. CHANGES TO THIS POLICY

13.1 We may update this Policy from time to time. If we make a material change,
we will give reasonable notice (for example, by email to your account address or
by a notice in the Service) before the change takes effect, and we will update
the "Effective" date.

13.2 Where a change would constitute a new purpose or disclosure that requires
your consent under applicable law, we will seek that consent before the change
takes effect.

14. CONTACT

Operator:        [OPERATOR_LEGAL_NAME]
Privacy contact: [PRIVACY_EMAIL]
Security contact:[SECURITY_EMAIL]
Address:         [REGISTERED_ADDRESS]

================================================================
END OF DRAFT PRIVACY POLICY
================================================================
