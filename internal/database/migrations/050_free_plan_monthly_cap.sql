-- TR-003 (docs/pre_saas_release_review.md): the public free beta needs a
-- finite default monthly request allowance. The free plan was seeded with the
-- unlimited sentinel (-1) for the private alpha (042_plans.sql); this
-- migration gives it a 20,000/month default.
--
-- Deliberately conditional: an operator who has already set a cap on the free
-- plan keeps it, so this only replaces the untouched seed (or a pre-migration
-- build of this branch). Change the cap at any time from the platform
-- dashboard — data-only and effective immediately; the enforcement path is
-- already in place.
UPDATE plans
SET monthly_requests = 20000,
    updated_at = '2026-10-04T00:00:00.000000000Z'
WHERE name = 'free' AND monthly_requests = -1;
