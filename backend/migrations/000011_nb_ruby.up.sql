-- 生词本句子补振り仮名：ruby_tokens 由 kagome 分词生成（与课程句子同一套 FR-2.12 管线）。
BEGIN;

ALTER TABLE notebook_sentence ADD COLUMN IF NOT EXISTS ruby_tokens JSONB;

COMMIT;
