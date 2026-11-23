-- 背词轮换与记忆钩子：
-- word_quiz_state 记住每词上次考过的例句，下次同质量档优先换一句（不降质量）；
-- word_hook 缓存按需生成的助记联想，错满 2 次的词自动生成，其余手动触发。
BEGIN;
CREATE TABLE word_quiz_state (
    word_id    TEXT PRIMARY KEY REFERENCES word (id) ON DELETE CASCADE,
    last_sent  TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE word_hook (
    word_id    TEXT PRIMARY KEY REFERENCES word (id) ON DELETE CASCADE,
    payload    JSONB NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
COMMIT;