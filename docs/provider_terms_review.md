# Hosted provider authentication policy review

`sass_tech.md` §14.3 and §40 require that each provider's terms are reviewed for
**hosted** use before that authentication mode is enabled in hosted Tiller.
Technical support for a flow does not imply hosted enablement, and API-key BYOK
and OAuth/subscription are separate enablement decisions.

This file is the working register. Fill the `Status` column per provider before
enabling that mode hosted. Hosted mode must default to **disabled** for any mode
whose status is not `allowed`.

| Provider / mode | Auth mode | Key-proxying permitted? | Hosted enablement | Notes |
|---|---|---|---|---|
| openai | API key (BYOK) | TBD | TBD | Check current terms for proxying a user-supplied key. |
| anthropic | API key (BYOK) | TBD | TBD | Check current terms. |
| gemini | API key (BYOK) | TBD | TBD | Check current terms. |
| codex (subscription) | OAuth | TBD | TBD | Subscription use through a third-party router is the high-risk case; review carefully. |
| github copilot | OAuth/device | TBD | TBD | Check developer policy for third-party routing. |
| opencode-free | API key | TBD | disabled | Hidden in the hosted add-provider UI via `Descriptor.HostedDisabled` (2026-10-01); terms review still separate. |
| opencode-zen | API key | TBD | TBD | Provider-scoped compatibility overrides already exist; terms review separate. |
| generic-openai / vllm | API key (BYOK) | TBD | TBD | User's own endpoint; still confirm no resale restriction is implied. |

## Review procedure

For each provider and each authentication mode:

1. Read the current provider terms / developer policy.
2. Confirm whether using that credential through a third-party hosted router is
   permitted, and whether account/credential sharing is restricted.
3. Record the finding here, with the date reviewed.
4. If unclear or incompatible, set hosted enablement to `disabled` for that
   mode. It may still remain available in self-hosted Tiller.

## Enforcement

Hosted enablement is a product/ops state, not (yet) a schema flag. The intended
enforcement is twofold: do not offer the mode in the hosted UI **and** reject
its provider type at hosted provider creation. `hostednet.Validate` already
blocks non-public endpoints; provider-type policy is a separate gate.

As of 2026-10-01 only the first half exists, and only for `opencode-free`:
`Descriptor.HostedDisabled` (`internal/providers/registry.go`) is set on that
one type, and the admin add-provider picker filters it out when
`runtimeMode === 'hosted'` (`internal/web/assets/app.js`). This is UI policy
only — `POST /api/admin/providers` (`internal/server/admin_providers.go`) does
not yet reject a `HostedDisabled` type, so it remains reachable with a direct
API call. Adding that server-side reject is a deliberate, tracked follow-up, not
an oversight.

Note that `HostedDisabled` is a create-time presentation flag: it does not
affect a provider row that already exists in a hosted database (for example one
carried over from a copied SQLite file), which continues to route normally until
an admin deletes it.

## Status legend

- `allowed` — reviewed, hosted use permitted.
- `disabled` — reviewed, not permitted or unclear; do not enable hosted.
- `TBD` — not yet reviewed; treat as `disabled` for hosted.

## Launch decision (Ben, 2026-11-04)

For the hosted launch, the unreviewed rows above are **not** blocked in code. The
OAuth/subscription modes (codex, claude-subscription, github-copilot) stay
available, and hosted does not reject a base URL that points at a
`HostedDisabled` provider's default host. Ben accepts this as end-user risk: the
user brings their own account or key. The intent of `HostedDisabled` is only
"do not offer it as an easy option" in the add-provider UI. This supersedes the
"treat TBD as disabled" legend for these launch cases; it does not mark any row
`allowed`.
