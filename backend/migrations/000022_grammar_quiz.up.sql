-- 语法本测验：题目一次生成入库，做题时不再调用大模型。
-- 题目内容与语法本同源（cloze 题的 stem 由原例句挖空重建）。
BEGIN;

CREATE TABLE grammar_question (
    id         TEXT PRIMARY KEY,
    point_id   TEXT NOT NULL REFERENCES grammar_point (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,              -- cloze=单句完形 / form=接续变形 / discriminate=近义辨析 / expression=情景表达
    stem       TEXT NOT NULL,              -- 题干，目标处为 ＿＿＿
    stem_ruby  JSONB NOT NULL DEFAULT '[]'::jsonb,
    options    JSONB NOT NULL,             -- [{text, reading, correct, why}]，固定顺序，接口下发时乱序
    explain    TEXT NOT NULL DEFAULT '',
    source_example_id TEXT REFERENCES grammar_example (id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'ok', -- ok=上线 / draft=待修
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (point_id, kind, stem)
);
CREATE INDEX grammar_question_point_idx ON grammar_question (point_id, status);

CREATE TABLE grammar_attempt (
    id          BIGSERIAL PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    question_id TEXT NOT NULL REFERENCES grammar_question (id) ON DELETE CASCADE,
    chosen      INT NOT NULL,
    correct     BOOLEAN NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX grammar_attempt_user_idx ON grammar_attempt (user_id, created_at DESC);

COMMIT;
