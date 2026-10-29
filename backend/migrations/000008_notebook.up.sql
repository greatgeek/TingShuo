-- 生词本：用户可建多个本；本内多级目录树；目录挂全局 word（去重不重复背）；词绑定句子。
-- 词进全局 word 表 ⇒ 背词 SRS / 生词库 / 复习 / 词发音缓存自动生效。
BEGIN;

CREATE TABLE notebook (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES "user" (id),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

CREATE TABLE notebook_dir (
    id          TEXT PRIMARY KEY,
    notebook_id TEXT NOT NULL REFERENCES notebook (id) ON DELETE CASCADE,
    parent_id   TEXT REFERENCES notebook_dir (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 同级目录同名唯一（parent 可空，用 COALESCE 归一）
CREATE UNIQUE INDEX ux_nbdir_sibling ON notebook_dir (notebook_id, COALESCE(parent_id, ''), name);

CREATE TABLE notebook_word (
    id         TEXT PRIMARY KEY,
    notebook_id TEXT NOT NULL REFERENCES notebook (id) ON DELETE CASCADE,
    dir_id     TEXT NOT NULL REFERENCES notebook_dir (id) ON DELETE CASCADE,
    word_id    TEXT NOT NULL REFERENCES word (id) ON DELETE CASCADE,
    sort_order INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dir_id, word_id)
);
CREATE INDEX idx_nbword_word ON notebook_word (word_id);

CREATE TABLE notebook_sentence (
    id               TEXT PRIMARY KEY,
    notebook_word_id TEXT NOT NULL REFERENCES notebook_word (id) ON DELETE CASCADE,
    text             TEXT NOT NULL,
    translation      TEXT,
    src              TEXT,                 -- conversation / grammar / listening / llm
    sort_order       INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (notebook_word_id, text)
);
CREATE INDEX idx_nbsent_word ON notebook_sentence (notebook_word_id);

COMMIT;
