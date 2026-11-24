BEGIN;

DROP INDEX IF EXISTS grammar_question_tag_idx;
ALTER TABLE grammar_question DROP COLUMN IF EXISTS tag;

COMMIT;