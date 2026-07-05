-- 001_initial.sql: baseline migration matching the current ragfs metadata
-- storage layout.
--
-- ragfs currently persists metadata via filesystem sidecar files
-- (.abstract, .overview, .chunks/, .sync_log.json) and does NOT store
-- per-resource metadata in SQL. This migration is therefore a no-op
-- placeholder so the migration runner can record a baseline version in
-- _migrations; future migrations will ALTER a real schema once one is
-- introduced.
SELECT 1;
