BEGIN;
ALTER TABLE word_clip_queue
    DROP COLUMN IF EXISTS expand_pending,
    DROP COLUMN IF EXISTS expanded;
COMMIT;
