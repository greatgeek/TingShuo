-- 查词：JMdict 词条 + LLM 中文释义/例句缓存 + 中文反查缓存 + 兜底词条。
-- 词条数据全局共享（不分用户）；中文内容按词条缓存，避免重复调用 LLM。
CREATE TABLE dict_entry (
    id         BIGINT PRIMARY KEY,             -- JMdict 词条号
    senses     JSONB NOT NULL,                 -- JMdict sense 数组（英文 gloss、词性、领域等）
    common     BOOLEAN NOT NULL DEFAULT false, -- 任一表记/读音带 common 标记
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dict_form (
    entry_id BIGINT NOT NULL REFERENCES dict_entry (id) ON DELETE CASCADE,
    form     TEXT NOT NULL,
    kind     CHAR(1) NOT NULL CHECK (kind IN ('k', 'r')), -- k=汉字表记 r=假名
    common   BOOLEAN NOT NULL DEFAULT false,
    tags     TEXT[] NOT NULL DEFAULT '{}',     -- rK/iK/uk 等表记备注
    PRIMARY KEY (entry_id, form, kind)
);
CREATE INDEX dict_form_form_idx ON dict_form (form text_pattern_ops);
CREATE INDEX dict_form_common_idx ON dict_form (form) WHERE common;

-- LLM 生成的中文释义与例句；src_hash 是英文 sense 的指纹，词条内容更新后可据此重翻。
CREATE TABLE dict_zh (
    entry_id    BIGINT PRIMARY KEY REFERENCES dict_entry (id) ON DELETE CASCADE,
    payload     JSONB NOT NULL,                -- {senses:[{pos,zh:[...]}], examples:[{jp,zh,pairs}]}
    search_text TEXT NOT NULL DEFAULT '',      -- 中文义项拼接，供中文反查
    model       TEXT NOT NULL DEFAULT '',
    src_hash    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 中文反查结果缓存：q 为归一化查询串。
CREATE TABLE dict_reverse (
    q          TEXT PRIMARY KEY,
    entry_ids  BIGINT[] NOT NULL DEFAULT '{}',
    model      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 词典里查不到的词（专有名词、新词）由 LLM 兜底生成的词条，按查询串缓存。
CREATE TABLE dict_fallback (
    q          TEXT PRIMARY KEY,
    payload    JSONB NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);