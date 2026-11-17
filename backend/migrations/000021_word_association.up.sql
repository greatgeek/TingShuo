-- 生词「联想」缓存：同词重复打开直接读缓存，不再请求大模型。
BEGIN;
CREATE TABLE word_association (
    word_id    TEXT PRIMARY KEY REFERENCES word (id) ON DELETE CASCADE,
    payload    JSONB NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
COMMIT;
