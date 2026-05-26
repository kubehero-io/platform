-- Seed the "default" org so RegisterCluster can persist when callers
-- don't (yet) supply an explicit org. Once the org-management RPCs
-- ship, customers create their own orgs and pass the resulting UUID
-- directly; this row stays for development + the kind-demo profile.
--
-- Idempotent — re-running migrations on an existing database is a no-op.

INSERT INTO orgs (slug, name, plan)
VALUES ('default', 'Default org', 'cloud-free')
ON CONFLICT (slug) DO NOTHING;
