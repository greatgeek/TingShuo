-- 播客源扩展到生词本目录（v0.3.1）：单元的词+句子作为播客原料
ALTER TABLE podcast_episode ADD COLUMN dir_id TEXT REFERENCES notebook_dir (id);
CREATE INDEX idx_podcast_user_dir ON podcast_episode (user_id, dir_id, created_at DESC);
