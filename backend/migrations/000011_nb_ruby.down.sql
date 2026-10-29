BEGIN;
ALTER TABLE notebook_sentence DROP COLUMN IF EXISTS ruby_tokens;
COMMIT;
