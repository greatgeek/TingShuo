DROP INDEX IF EXISTS idx_podcast_user_dir;
ALTER TABLE podcast_episode DROP COLUMN IF EXISTS dir_id;
