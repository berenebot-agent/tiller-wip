*** DRAFT FOR LEGAL REVIEW ***

This document is an engineering first draft prepared for legal review. It has
not been reviewed or approved by a qualified lawyer. It contains unresolved
[PLACEHOLDER] tokens. Do not publish it to end users or rely on it as legal
advice. Do not treat any part of it as final until a lawyer has signed off.

================================================================
PRIVACY POLICY
================================================================

Operator:    [OPERATOR_LEGAL_NAME]
ABN:         [ABN]
Address:     [REGISTERED_ADDRESS]
Privacy contact: [PRIVACY_EMAIL]
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

1.4 This Policy should be read with our Terms of Service, Acceptable Use Policy,
Subprocessor List and Security and Data Handling page.

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

3.2 We use only strictly necessary authentication and security cookies. We do
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
  (f) comply with law and enforce our Terms of Service and Acceptable Use
      Policy; and
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

7.2 The Service cannot recover encrypted credentials if the master key is lost;
see the Security and Data Handling page.

8. SUBPROCESSORS AND OVERSEAS PROCESSING AND DISCLOSURE

8.1 We use a small number of third-party service providers ("subprocessors") to
operate the Service. These currently include our cloud/compute host,
transactional email provider, and DNS/TLS/edge provider, and may include an
error-monitoring provider. The current list, with purpose, data categories,
whether data is persisted or transient, country of processing, and transfer
mechanism, is published in the Subprocessor List.

8.2 We are an Australian-operated service. Depending on the subprocessor and
your location, personal information may be processed or disclosed outside
Australia, including in the United States. We disclose those likely locations
rather than making a vague "anywhere" statement.

8.3 Where personal information is transferred from the European Economic Area,
the United Kingdom or another jurisdiction with transfer restrictions, we rely
on an appropriate transfer mechanism for the relevant subprocessor (for example
an adequacy decision, the EU-US Data Privacy Framework where the recipient is
eligible, Standard Contractual Clauses, the UK Extension to the EU-US Data
Privacy Framework, or the UK IDTA/Addendum). Transfer positions are reassessed
when subprocessors change. Details are in the Subprocessor List.

8.4 We do not disclose personal information to any other third party except:
(a) with your direction (for example, routing your request to the provider you
configure); (b) to subprocessors listed in the Subprocessor List; (c) where
required by law or to respond to lawful requests; or (d) to protect the rights,
safety and security of the Service, our users or others.

8.5 When you configure a provider, request content is sent to that provider
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
information, including encryption of provider credentials at rest, hashed
storage of secrets, transport encryption (HTTPS/TLS), access controls, and
outbound network restrictions. A fuller summary is on the Security and Data
Handling page.

10.2 No system is perfectly secure. If a data breach occurs that is likely to
result in serious harm and meets the threshold under the Notifiable Data
Breaches scheme, we will notify affected individuals and the Office of the
Australian Information Commissioner as required by law. Where EU/UK breach
notification obligations apply, we will comply with those as well.

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
