-- 收藏（FR-12.3：句子收藏为句卡，可单独听/连播；材料级收藏标记）
BEGIN;

CREATE TABLE favorite_sentence (
    user_id     TEXT NOT NULL REFERENCES "user" (id),
    sentence_id TEXT NOT NULL REFERENCES sentence (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, sentence_id)
);
CREATE INDEX idx_fav_sent_user ON favorite_sentence (user_id, created_at DESC);

CREATE TABLE favorite_material (
    user_id     TEXT NOT NULL REFERENCES "user" (id),
    material_id TEXT NOT NULL REFERENCES material (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, material_id)
);

COMMIT;
