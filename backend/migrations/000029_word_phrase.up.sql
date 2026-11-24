-- 生词常用词组缓存：每词 5 条短搭配（日文+假名+中文，优先动宾），供生词本词卡展开区展示。
BEGIN;

CREATE TABLE IF NOT EXISTS word_phrase (
    word_id    TEXT PRIMARY KEY REFERENCES word(id) ON DELETE CASCADE,
    payload    JSONB NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;