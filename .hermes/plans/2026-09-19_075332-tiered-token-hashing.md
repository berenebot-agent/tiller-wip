# Stage 2 — Entropy-tiered token hashing (bcrypt for machine tokens, argon2id for the admin credential)

**Goal:** Remove the 64 MiB-per-verify memory transient (the dominant driver from
Probe #2) without weakening protection of the one genuinely low-entropy secret.

**Authorised deviation:** `AGENTS.md` previously required a memory-hard KDF for
*all* secrets. This change narrows that: a memory-hard KDF (argon2id) remains
required for the low-entropy admin credential, while high-entropy machine tokens
(client API keys, admin session tokens) use bcrypt. Rationale is written into
`AGENTS.md` and `SECURITY.md`.

---

## Rationale (evidence from Probe #2)

- argon2id `initBlocks` = 96% of cumulative allocations; each in-flight verify
  holds a 64 MiB buffer.
- Four concurrent verifies inflate the GC live set to `198 MB` (gctrace
  `198->198->197 MB, 198 MB goal`), so GOGC paces the next collection at ~198 MB
  and the RSS parks at ~170–300 MB.
- Client keys and session tokens are 256-bit uniformly random; offline brute
  force is infeasible with any sane hash, so memory-hardness buys ~nothing there.
- The admin credential fingerprint (`settings.admin_credential_hash`) is the only
  low-entropy, human-chosen secret at rest → argon2id stays for it.
- bcrypt ships in the already-required `golang.org/x/crypto`; no new dependency.

## Design

| Secret | Before | After |
|---|---|---|
| Admin credential fingerprint | argon2id | argon2id (unchanged) |
| Admin session `token_hash` | argon2id | bcrypt cost 10 |
| Client key `secret_hash` | argon2id | bcrypt cost 10 |

`bcryptCost = 10` is a package constant (not env-tunable), per decision.

**Algorithm-aware verification.** Legacy rows are argon2id PHC strings. Verify
dispatches on prefix (`$argon2id$` vs `$2a$/$2b$/$2y$`) so existing keys and
sessions keep working. `NeedsRehash` reports whether an encoded hash should be
upgraded to the current format/cost.

**Lazy rehash-on-verify.** After a successful verify of a legacy argon2 hash, the
row is optimistically rewritten as bcrypt:
`UPDATE ... SET secret_hash=? WHERE id=? AND secret_hash=?` (compare-and-swap;
best-effort, never fails the auth path). Sessions upgrade on next login; client
keys on next successful auth. Zero-downtime, no bulk migration job.

## Changes

1. `internal/auth/secret_hasher.go`
   - Add `NeedsRehash(encoded string) bool` to `SecretHasher`.
   - Add `BcryptHasher{Cost int}`; `Hash` uses bcrypt; `Verify` dispatches
     bcrypt/legacy-argon2; `NeedsRehash` true for non-bcrypt or wrong cost.
   - `Argon2Hasher.NeedsRehash` returns false (deliberate format).
   - Add package `VerifyEncoded` helper for the prefix dispatch.
2. `internal/auth/keys.go`
   - `ClientAuthenticator.AuthenticateContext`: lazy rehash after success.
   - `SessionStore` gains `tokenHasher` + `credentialHasher`; add
     `NewSessionStoreTiered`; keep `NewSessionStoreWithHasher` (same hasher for
     both, tests) and make `NewSessionStore` tiered (bcrypt + argon2).
   - `Create`/`Get`/`Validate` use the token hasher; `syncCredential` uses the
     credential hasher. Lazy rehash in `Get`/`Validate`.
   - Production `NewClientAuthenticator` uses `BcryptHasher`.
3. `internal/server/server.go`
   - `serverOptions` carries `tokenHasher` + `credentialHasher`; `withSecretHasher`
     sets both (tests unchanged); production defaults bcrypt + argon2.
   - `Server.secretHasher` stays the token hasher (used by
     `GenerateKeyWithHasher`).
4. `internal/testutil/fastsecret/hasher.go`: `NeedsRehash` returns false.
5. Docs: `AGENTS.md` guardrail reworded; `SECURITY.md` gains a tiering note.
6. Tests: bcrypt round-trip; dual-verify accepts legacy argon2; `NeedsRehash`;
   rehash-on-auth rewrites the row; session interop; production assertions
   updated (token=bcrypt, credential=argon2id).

## Verification (full tests approved)

`./tests/scripts/check-fmt.sh`, `./tiller-go.sh vet ./...`,
`./tiller-go.sh test ./...`. Rebuild the container and re-run the Probe #2 burst
to confirm the peak collapse (expected: no ~198 MB live-set inflation).

## Rollback

Revert the commit; legacy argon2 rows still verify via prefix dispatch either
way, so the change is behaviourally safe in both directions.