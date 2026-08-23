-- 0003_active_grab_guard.sql
-- A user action must not create another qBittorrent handoff while the same
-- item is already downloading or importing. The repository checks this first;
-- this partial index closes the race across multiple Reelay processes.

CREATE UNIQUE INDEX idx_grabs_one_active_subject
ON grabs (subject_type, subject_id)
WHERE state IN ('pending', 'downloading', 'completed', 'importing');
