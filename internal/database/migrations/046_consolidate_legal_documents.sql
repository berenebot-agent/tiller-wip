-- Consolidate the hosted legal pack to the two documents users actually accept:
-- Terms of Service (acceptable-use rules incorporated) and Privacy Policy
-- (subprocessor list and security/data-handling disclosures incorporated).
--
-- Earlier deploys seeded and exposed a separate Acceptable Use Policy,
-- Subprocessor List, Security and Data Handling page, and signup notice. The
-- signup notice was never published (it was a collection-notice resource), but
-- remove it defensively. Acceptance history in legal_acceptances is untouched:
-- it records agreement to the Terms and Privacy Policy only.
DELETE FROM legal_documents
WHERE slug NOT IN ('terms', 'privacy');
