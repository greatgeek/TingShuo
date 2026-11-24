-- 语法题库增加专属套题标记：tag 为空表示普通题，非空表示只在对应套题里抽取。
BEGIN;

ALTER TABLE grammar_question ADD COLUMN tag TEXT NOT NULL DEFAULT '';
CREATE INDEX grammar_question_tag_idx ON grammar_question (tag) WHERE tag <> '';

COMMIT;