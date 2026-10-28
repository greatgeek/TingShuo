-- 背词（词级 SRS）：与句子复习（review_item）并行独立。
-- 一个词有没有这张卡 = 新学候选（无卡）/ 复习候选（有卡且未掌握）。
BEGIN;

CREATE TABLE word_review_item (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES "user" (id),
    word_id      TEXT NOT NULL REFERENCES word (id),
    interval_days INT NOT NULL DEFAULT 1,
    due_date     DATE NOT NULL DEFAULT CURRENT_DATE + 1,
    error_count  INT NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'learning' CHECK (status IN ('learning', 'mastered')),
    last_result  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (word_id)  -- 一词一卡（词属于唯一用户，故词级唯一即用户级唯一）
);
CREATE INDEX idx_wri_user_due ON word_review_item (user_id, status, due_date);

COMMIT;
