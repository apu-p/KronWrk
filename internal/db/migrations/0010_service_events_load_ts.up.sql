-- Rename service_events.created_at to load_ts. The idx_service_events_service
-- index references the column by position, so it follows the rename; nothing
-- in the app reads the column (the insert relies on its DEFAULT now()).
ALTER TABLE service_events RENAME COLUMN created_at TO load_ts;
