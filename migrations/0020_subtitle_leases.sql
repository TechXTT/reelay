ALTER TABLE subtitle_jobs ADD COLUMN lease_token TEXT NOT NULL DEFAULT '';
ALTER TABLE subtitle_coverage ADD COLUMN follow_pending INTEGER NOT NULL DEFAULT 0 CHECK (follow_pending IN (0, 1));
