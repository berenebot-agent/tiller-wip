# Tiller Router — SaaS / Multi-User Roadmap

**Project:** Tiller Router  
**Status:** Deferred architecture roadmap  
**Scope:** Multi-user, workspace, hosted and SaaS evolution  
**Prerequisite:** Core Tiller Router functionality should be proven first. See `tiller-router-roadmap-v2-core.md`.

This roadmap preserves the multi-user/SaaS architecture thinking without burdening the current router implementation.

---

# 1. Trigger for Starting This Roadmap

Do not start this roadmap simply because multi-tenancy is technically possible.

Begin only when:

- Tiller is useful in sustained real-world operation;
- multiple clients/tools rely on it;
- routing/fallback behaviour is mature;
- there is evidence that other people or organisations would benefit;
- hosted operation would remove meaningful deployment burden.

---

# 2. Likely SaaS Product

The most natural first hosted product is:

> Hosted Tiller Router where each user/workspace supplies its own upstream provider credentials.

This means Tiller operates the control plane, not the model inference balance sheet.

Benefits:

- no need to pre-fund inference;
- user owns provider relationship;
- easier pricing model;
- avoids initially becoming an OpenRouter competitor;
- provider choice remains broad;
- customer can bring corporate/provider-specific accounts.

Potential progression:

```text
Self-hosted Tiller
      ↓
Hosted personal BYO-key
      ↓
Hosted team/workspace
      ↓
Managed enterprise routing
```

---

# 3. Long-Term Ownership Model

Future hierarchy:

```text
Tiller Router installation
    |
    +-- Users
    |
    +-- Workspaces
           |
           +-- memberships
           +-- provider instances
           +-- provider credentials
           +-- provider catalogues
           +-- virtual provider groups/models
           +-- client API keys
           +-- request activity
           +-- workspace settings
           +-- quotas / plan metadata
```

A human user is distinct from a machine client API key.

---

# 4. User Accounts

Future table:

```text
users
-----
id
username_or_email
display_name
password_hash
enabled
system_admin
created_at
updated_at
last_login_at
```

Potential authentication sources:

- local password;
- Google;
- Microsoft;
- GitHub;
- generic OIDC.

Self-hosted deployments should continue to support local DB-backed users.

---

# 5. Workspaces

Future table:

```text
workspaces
----------
id
name
status
created_at
updated_at
```

All tenant-owned routing resources become workspace scoped.

---

# 6. Workspace Membership

Future table:

```text
workspace_memberships
---------------------
user_id
workspace_id
role
created_at
updated_at
```

Candidate roles:

```text
owner
admin
operator
viewer
```

Do not implement complex RBAC before real use cases require it.

---

# 7. System Admin vs Workspace Role

Keep these separate.

A future SaaS operator may be:

```text
system_admin = true
```

without being an ordinary customer workspace member.

A customer may be:

```text
Workspace Owner
system_admin = false
```

This avoids conflating product/operator administration with customer ownership.

---

# 8. Tenant-Owned Data

At SaaS transition, at minimum scope these entities:

```text
providers.workspace_id
virtual_provider_groups.workspace_id
client_keys.workspace_id
request_logs.workspace_id
workspace_settings.workspace_id
```

Provider models inherit workspace via provider.

Virtual models inherit workspace via virtual group.

Explicit `workspace_id` may be added to high-volume tables such as logs for safe/simple querying.

---

# 9. Tenant Isolation Rule

Workspace ownership is a security boundary.

Never rely on front-end filtering alone.

Every tenant-owned API read/mutation must validate:

```text
authenticated user
    ↓
workspace membership
    ↓
workspace id
    ↓
resource ownership
```

Automated tests should attempt cross-workspace access for every relevant endpoint.

---

# 10. Naming Scope

At multi-workspace transition, provider and virtual names become workspace-scoped.

Valid:

```text
Workspace A:
  openrouter
  main/coding

Workspace B:
  openrouter
  main/coding
```

Canonical model IDs only need uniqueness within the workspace visible to that client.

---

# 11. First-Run Experience for Self-Hosted Multi-User Version

If/when Tiller adopts real human accounts for self-hosted deployments, prefer HTML first-run account creation rather than permanent environment credentials.

Fresh instance:

```text
Tiller Router — First Run Setup

Username / email
Display name
Password
Confirm password

[ Create Administrator ]
```

Atomic setup:

1. verify uninitialised;
2. create first user;
3. Argon2id password hash;
4. create Default Workspace;
5. create owner membership;
6. set system-admin privilege;
7. mark instance initialised;
8. begin authenticated session.

First-run setup permanently closes after successful initialisation.

---

# 12. Break-Glass Recovery

First-run setup must not double as account recovery.

Host-only CLI recovery:

```text
tiller-router admin reset-password --username <user>
```

Additional recovery:

```text
tiller-router admin create-system-admin --username <user>
tiller-router admin grant-system-admin --username <user>
```

No permanent web recovery backdoor.

No old bootstrap environment password.

Root/Docker access to the host is the self-hosted break-glass authority.

---

# 13. Registration Toggle

Future installation-level setting:

```text
registration_enabled = true | false
```

When OFF:

- no public signup.

When ON:

- signup route available.

But registration must not be implemented until tenant isolation is proven.

---

# 14. Signup Policy

Need explicit policy choices.

Possible modes:

```text
invite_only
signup_creates_workspace
signup_requires_existing_workspace_invite
open_signup
```

Recommended first hosted mode:

```text
signup_creates_workspace
```

with email verification and abuse controls.

---

# 15. Invitations

Team/workspace mode likely needs:

- invite by email;
- invite expiry;
- role assignment;
- accept/decline;
- resend/revoke.

Avoid invitations until basic user/workspace ownership is stable.

---

# 16. Workspace Switching

A user may eventually belong to multiple workspaces.

UI should support:

```text
Current Workspace ▼
```

All admin UI/API operations occur inside the selected workspace context.

System administration should be a separate operator surface.

---

# 17. Provider Credentials in SaaS

Provider credentials become particularly sensitive in hosted operation.

Before SaaS:

- encrypted at rest;
- master encryption key outside DB;
- strict workspace isolation;
- never re-display plaintext;
- replacement only;
- admin/system operator should not casually surface customer credentials.

Consider whether system operators should technically be able to decrypt credentials at all.

---

# 18. Activity / Usage in Multi-Tenant Mode

Every request log should be workspace scoped.

Future views:

- per client;
- per workspace;
- per virtual model;
- provider/model usage;
- failures;
- latency;
- token usage.

Still no prompt/response persistence by default.

---

# 19. SaaS Security Prerequisites

Before public signup:

- proven workspace isolation;
- tenant-isolation test suite;
- provider credential encryption;
- secure password storage;
- session hardening;
- CSRF protection;
- auth rate limiting;
- email verification or OAuth/OIDC;
- password reset/account recovery;
- abuse protection;
- backup/restore tested with multiple workspaces;
- account deletion;
- workspace deletion;
- audit trail for sensitive admin actions;
- security review of all workspace-scoped DB/API paths.

---

# 20. Account Recovery

Hosted SaaS cannot depend on host CLI for ordinary customer recovery.

Need:

- email verification;
- forgot-password flow;
- reset token expiry;
- session invalidation after reset;
- recovery rate limits;
- possible MFA later.

Host CLI remains operator break-glass for self-hosted installs.

---

# 21. OAuth / OIDC

Potential:

- Google;
- Microsoft;
- GitHub;
- generic OIDC.

For business customers, Microsoft/Google/OIDC may be more valuable than expanding native password features.

---

# 22. Workspace Roles

Initial roles:

```text
owner
admin
operator
viewer
```

Possible semantics:

### owner

- billing/plan;
- workspace delete;
- ownership transfer;
- full configuration.

### admin

- providers;
- virtual models;
- client keys;
- users/memberships.

### operator

- routing/model changes;
- activity;
- perhaps client permissions.

### viewer

- read-only.

Do not over-engineer fine-grained permissions initially.

---

# 23. Hosted Plans

Possible later product structure:

```text
Free
Personal
Team
Business
```

But pricing should be based on observed operational cost, not speculation.

Potential charging dimensions:

- workspace;
- number of client keys;
- number of provider instances;
- request volume;
- advanced routing features;
- retention period;
- team users.

Avoid token-margin billing if users BYO provider credentials.

---

# 24. Quotas

Hosted operation may require:

- requests/minute;
- concurrent requests;
- monthly request allowance;
- request-log retention limits;
- provider count;
- client-key count;
- workspace-member count.

Quota infrastructure should remain separate from routing permission semantics.

---

# 25. Billing

Only implement if hosted usage justifies it.

Possible Stripe-style subscription integration later.

Do not let billing logic contaminate the router core.

Suggested separation:

```text
plan entitlement
    ↓
management/API limits

routing core
    remains deterministic
```

---

# 26. Operator Administration

Hosted Tiller will need a system/operator view distinct from customer workspace UI.

Possible capabilities:

- users/workspaces;
- suspension;
- plan;
- global health;
- system backups;
- abuse events;
- aggregate operational metrics.

Operator access to customer provider credentials should remain restricted.

---

# 27. Workspace Suspension

Hosted mode needs safe suspension semantics.

Possible states:

```text
active
suspended
disabled
pending_deletion
```

Suspended workspace:

- admin login may be limited;
- inference keys rejected;
- data preserved.

Do not delete automatically because of transient billing/account state.

---

# 28. Workspace Export / Deletion

Future privacy/admin requirements:

### Export

- provider definitions;
- virtual models;
- key metadata;
- permissions;
- activity metadata;
- settings.

Plaintext client keys cannot be exported because they are hash-only.

Provider credentials should require explicit handling/exclusion.

### Delete

- explicit confirmation;
- grace period if hosted;
- revoke client keys;
- destroy provider credentials;
- purge request logs;
- backup-retention policy considered.

---

# 29. Hosted Architecture

Initially prefer keeping the same simple application architecture where possible:

```text
Tiller Router
  + SQLite
```

For small hosted deployments this may remain sufficient.

Only move to PostgreSQL/Redis/etc. after measured concurrency or HA requirements justify it.

Do not prematurely replace the self-hosted architecture just because SaaS exists.

---

# 30. When SQLite Stops Being Enough

Potential triggers:

- multiple application replicas;
- high concurrent write volume;
- HA requirement;
- cross-region operation;
- database size/backup constraints;
- measurable lock contention.

Until then, SQLite simplicity is valuable.

---

# 31. SaaS Observability

Hosted operation needs system-level operational metadata:

- request counts;
- errors;
- latency;
- workspace request rates;
- provider health;
- auth failures;
- system resource health.

Still avoid storing customer prompt/response content unless a future opt-in product explicitly requires it.

---

# 32. Abuse Protection

Public signup requires:

- signup throttling;
- login throttling;
- API abuse controls;
- workspace quotas;
- suspicious client-key activity handling;
- provider proxy abuse prevention.

A BYO-key model reduces some financial abuse risk but not infrastructure abuse.

---

# 33. Domain / Branding

Potential hosted forms:

```text
app.tillerrouter...
api.tillerrouter...
```

Each workspace may eventually receive:

- workspace slug;
- named client keys;
- optional custom endpoint/domain later.

Custom domains are not an early requirement.

---

# 34. Enterprise Possibilities

Only if demand emerges:

- SSO;
- SCIM;
- audit retention;
- organisation policy;
- approved-provider catalogue;
- mandatory virtual-model usage;
- compliance exports;
- private deployment;
- support/SLA.

Do not implement in advance.

---

# 35. SaaS Grill-Me Questions

Before starting this roadmap, revisit these decisions.

## 35.1 User model

1. Username or email as primary identity?
2. Local passwords at all, or OAuth/OIDC-first?
3. Can one email belong to multiple workspaces?
4. Can a user create multiple workspaces?
5. Do we need personal workspace + team workspaces?

---

## 35.2 Signup

1. Open signup?
2. Invite-only?
3. Signup creates a workspace automatically?
4. Email verification mandatory?
5. CAPTCHA/abuse protection?
6. Free trial?

---

## 35.3 Workspace ownership

1. Can ownership transfer?
2. Can multiple owners exist?
3. What happens if last owner leaves?
4. Can system operator assume workspace access?
5. Should operator access require explicit support consent?

---

## 35.4 Provider credentials

1. Can Tiller operators technically decrypt customer keys?
2. Do we want customer-controlled encryption keys?
3. Do we support secret-manager integrations?
4. Do credentials survive workspace export?
5. How are credentials destroyed on deletion?

---

## 35.5 Pricing

1. Charge per workspace?
2. Per client key?
3. Per user?
4. Per request?
5. Advanced features?
6. Logging retention?
7. Are self-hosted features identical to hosted?

---

## 35.6 SaaS database architecture

1. How many expected workspaces?
2. How many requests/sec?
3. Is single-instance SQLite still reasonable?
4. At what measured threshold do we move to Postgres?
5. Do we need multiple replicas?
6. Do we need regional deployment?

---

# 36. SaaS Definition of Readiness

Do not publicly launch until:

- core router is mature;
- fallbacks are predictable;
- logging works;
- provider secrets encrypted;
- users/workspaces fully isolated;
- account recovery works;
- multi-tenant backup/restore works;
- auth/abuse protections exist;
- operator/customer privilege boundaries are clear;
- deletion/export lifecycle is defined;
- hosting costs and likely pricing are understood.

---

# 37. Guardrail

SaaS should remain:

> Hosted Tiller Router.

It should not become:

> Every possible AI gateway, billing, agent, analytics and workflow feature in one product.

Keep the router core small and keep hosted/user-management concerns layered around it.
