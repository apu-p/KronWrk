-- Free-text comment captured when a job is created: a change-request id in an
-- enterprise setting (e.g. "CHG-1234"), or a note on what the job is for.
-- NOT NULL DEFAULT '' so existing rows and comment-less inserts stay simple.
-- No grant changes: SELECT on jobs is table-level for every reader role, and
-- only inserts set it (support's column-scoped UPDATE stays enabled-only).
ALTER TABLE jobs ADD COLUMN comment TEXT NOT NULL DEFAULT '';
