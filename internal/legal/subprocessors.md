*** DRAFT FOR LEGAL REVIEW ***

This document is an engineering first draft prepared for legal review. It has
not been reviewed or approved by a qualified lawyer. It contains unresolved
[PLACEHOLDER] tokens. The vendor names and countries below are placeholders that
must be replaced with the actual, contracted subprocessors before publication.
Do not publish it to end users or rely on it as legal advice. Do not treat any
part of it as final until a lawyer has signed off.

================================================================
SUBPROCESSOR LIST
================================================================

Operator:    [OPERATOR_LEGAL_NAME]
Effective:   [EFFECTIVE_DATE]

1. PURPOSE

1.1 This list describes the third-party service providers ("subprocessors")
that [OPERATOR_LEGAL_NAME] uses to operate the hosted service under the name
"tiller-router" ("Hosted Tiller"). It supports the Privacy Policy, which
describes why and how we handle personal information.

1.2 We are an Australian-operated service and users may be located anywhere in
the world. Some subprocessors may process personal information outside Australia,
including in the United States. The table below records the likely country of
processing and the transfer basis where relevant.

1.3 This list is reviewed before any new subprocessor that receives user data is
added. Adding such a subprocessor requires an update to this list and, where
appropriate, notice to users.

2. SUBPROCESSORS

The table is written for plain-text display. Columns are separated by a pipe
("|") and rows are grouped by purpose.

Purpose                | Vendor (placeholder) | Data categories handled                     | Persisted / transient        | Country of processing | Transfer mechanism
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
Cloud / compute host   | [CLOUD_VENDOR]       | Account information; Activity metadata;     | Persisted (encrypted at rest)| Australia and/or the  | Adequacy / SCC / DPF
                       |                      | encrypted provider credentials; operational |                              | United States         | as applicable (see §3)
                       |                      | logs (metadata only); transient request &  | Transient for prompt/        |                       |
                       |                      | response content                            | response content             |                       |
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
Transactional email    | [EMAIL_VENDOR]       | Email address; account event notifications; | Persisted by the vendor      | United States and/or  | SCC / DPF / UK
(provider TBD: Resend, | (e.g. Resend, Brevo, | verification / password-reset tokens        | for delivery; tokens         | the vendor's region   | Extension as applicable
Brevo or self-managed  | or SMTP operator)    |                                             | short-lived                  |                       | (see §3)
SMTP)                  |                      |                                             |                              |                       |
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
DNS / TLS / edge       | [EDGE_VENDOR]        | Request metadata at the network edge;       | Transient (metadata only)    | Global edge network   | Adequacy / SCC /
protection             |                      | connection metadata; TLS termination        |                              | (incl. Australia and  | DPF as applicable
                       |                      |                                             |                              | the United States)    | (see §3)
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
Error monitoring       | [MONITORING_VENDOR]  | Error and performance metadata (no prompt    | Persisted by the vendor      | United States         | SCC / DPF as
(if enabled)           |                      | or response bodies; no credentials)         | for a limited window         |                       | applicable (see §3)
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------
Billing (future, once  | [BILLING_VENDOR]     | Billing contact; payment metadata; plan      | Persisted by the vendor      | To be determined      | To be assessed
paid plans launch)     |                      | status                                      |                              |                       | before enablement
-----------------------|----------------------|---------------------------------------------|------------------------------|-----------------------|-------------------

3. TRANSFER MECHANISMS

3.1 Where personal information is transferred from the European Economic Area to
a country without an adequacy decision, we rely on Standard Contractual Clauses
with the relevant subprocessor (or another lawful Chapter V mechanism).

3.2 Where a US recipient is eligible under the EU-US Data Privacy Framework, we
may rely on that framework, and on the UK Extension to the EU-US Data Privacy
Framework for UK transfers where applicable.

3.3 Where the UK IDTA/Addendum or another UK safeguard is required, we put it in
place with the relevant subprocessor.

3.4 Transfer positions are reassessed when a subprocessor or its processing
location changes. Where a mechanism is still to be confirmed, it is marked
accordingly in the table and must be resolved before that subprocessor handles
personal information.

4. PROVIDERS YOU CONFIGURE ARE NOT SUBPROCESSORS

4.1 The AI model providers you connect with your own credentials are selected and
controlled by you. They are not our subprocessors. When you make a request, your
request content is sent to the provider you configured under that provider's own
terms and privacy policy.

5. CHANGES

5.1 We may update this list from time to time. Material changes will be
communicated as described in the Terms of Service.

6. CONTACT

Operator:        [OPERATOR_LEGAL_NAME]
Privacy contact: [PRIVACY_EMAIL]
Address:         [REGISTERED_ADDRESS]

================================================================
END OF DRAFT SUBPROCESSOR LIST
================================================================
