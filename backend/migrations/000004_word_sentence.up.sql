-- 词↔句子关联表：落库时从 word_occurrence 展开生成。
-- 能力：生词 → 其生成的短语（material.type='phrases'）/ 出现的句子（对话/短文），
-- 句子 → 场景经 sentence.material_id → material.scene JOIN 可达（不冗余）。
BEGIN;

CREATE TABLE word_sentence (
    word_id     TEXT NOT NULL REFERENCES word (id),
    sentence_id TEXT NOT NULL REFERENCES sentence (id),
    lesson_id   TEXT NOT NULL REFERENCES daily_lesson (id), -- 冗余：按课查询免 JOIN
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (word_id, sentence_id)
);
CREATE INDEX idx_wsent_word     ON word_sentence (word_id);
CREATE INDEX idx_wsent_sentence ON word_sentence (sentence_id);
CREATE INDEX idx_wsent_lesson   ON word_sentence (lesson_id, word_id);

COMMIT;
