-- =============================================================
-- TingShuo 初始 Schema（需求与设计文档 v1.5 §5.2）
-- ID 统一 ULID（TEXT，时间有序，便于同步游标）
-- 核心不变式：文本单元 = 音频单元 = 练习单元 = 复习单元 = 录音单元，全部挂在 sentence 上
-- =============================================================

BEGIN;

-- ---------- 用户（user 是保留字，需引号）----------
CREATE TABLE "user" (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    nickname        TEXT,
    target_language TEXT NOT NULL DEFAULT 'ja',   -- 'ja'/'en'/'ko'（P2 多语言预留，§12）
    level           TEXT CHECK (level IN ('N5', 'N4', 'N3')),
                    -- 语义随 target_language 映射：ja=JLPT / en=CEFR / ko=TOPIK（多语言落地改应用层校验）
    scene_prefs     JSONB,                          -- ["便利店","车站",...]
    style_prefs     JSONB,                          -- {"voice_a":"Nanami","voice_b":"Keita","passage":"Nanami","rate":1.0,"politeness":"masu_with_casual_mix"}
    tts_auto_gen    BOOLEAN NOT NULL DEFAULT true,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- 每日课程 ----------
CREATE TABLE daily_lesson (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES "user" (id),
    lesson_date     DATE NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('draft', 'generating', 'ready', 'failed')),
    word_count      INT,
    generate_params JSONB,                           -- 本次生成用的参数快照
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, lesson_date)                    -- 一天一课（一期简化，开放问题 Q1）
);

-- ---------- 单词（全局词库，跨课程）----------
CREATE TABLE word (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES "user" (id),
    surface    TEXT NOT NULL,                        -- 单词（汉字形）
    reading    TEXT,
    pos        TEXT,
    meaning    TEXT,
    mastery    TEXT NOT NULL DEFAULT 'new' CHECK (mastery IN ('new', 'key', 'learned', 'weak')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, surface, reading)
    -- 注意：reading 可空。PG 默认 NULL 互不相等，同词无读音可重复插入；
    -- 需严格唯一时应用层先补全读音，或升级为 NULLS NOT DISTINCT（PG15+）
);

-- ---------- 词单关联（词在课程中的快照状态）----------
CREATE TABLE lesson_word (
    lesson_id        TEXT NOT NULL REFERENCES daily_lesson (id),
    word_id          TEXT NOT NULL REFERENCES word (id),
    is_key           BOOLEAN NOT NULL DEFAULT false, -- 重点词（FR-2.2 出现 2 次）
    occurrence_count INT NOT NULL DEFAULT 0,
    skipped          BOOLEAN NOT NULL DEFAULT false, -- FR-2.8 允许 skipped 并说明原因
    skip_reason      TEXT,
    PRIMARY KEY (lesson_id, word_id)
);

-- ---------- 材料（对话或短文）----------
CREATE TABLE material (
    id         TEXT PRIMARY KEY,
    lesson_id  TEXT NOT NULL REFERENCES daily_lesson (id),
    type       TEXT NOT NULL CHECK (type IN ('dialogue', 'passage')),
    scene      TEXT,                                 -- 场景标签
    role_a     TEXT,                                 -- 对话角色名（短文为 NULL）
    role_b     TEXT,
    title      TEXT,
    sort_order INT NOT NULL DEFAULT 0
);

-- ---------- 句子（核心表，§5.1 全部活动挂在此）----------
CREATE TABLE sentence (
    id          TEXT PRIMARY KEY,
    material_id TEXT NOT NULL REFERENCES material (id),
    lesson_id   TEXT NOT NULL REFERENCES daily_lesson (id),
    sort_order  INT NOT NULL DEFAULT 0,
    speaker     TEXT,                                -- A/B 或 独白
    text        TEXT NOT NULL,                       -- 原文（日语含汉字）
    kana        TEXT,                                -- 整句假名串（可空）：仅作交叉校验参考，不直接展示（FR-2.12）
    ruby_tokens JSONB,                               -- 振り仮名结构：[["会計","かいけい"],["を",null],...]
    translation TEXT,                                -- 中文
    para_no     INT NOT NULL DEFAULT 0               -- 短文段落号（对话为 0）
);
CREATE INDEX idx_sentence_material ON sentence (material_id, sort_order);
CREATE INDEX idx_sentence_lesson ON sentence (lesson_id);

-- ---------- 音频资产（TTS 缓存与全局去重落点，FR-3.5）----------
CREATE TABLE audio_asset (
    id          TEXT PRIMARY KEY,
    sentence_id TEXT NOT NULL REFERENCES sentence (id),
    cache_key   TEXT NOT NULL UNIQUE,                -- hash(sentence_norm + voice + rate + style + engine_ver)
    voice       TEXT NOT NULL,
    rate        REAL NOT NULL DEFAULT 1.0,
    style       TEXT,
    format      TEXT NOT NULL DEFAULT 'mp3',
    duration_ms INT,
    size_bytes  INT,
    storage_url TEXT,                                -- 对象存储地址
    status      TEXT NOT NULL CHECK (status IN ('pending', 'ready', 'failed')),
    engine_ver  TEXT,
    generated_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audio_sentence ON audio_asset (sentence_id);

-- ---------- 练习流水（append-only，同步友好，FR-9.3）----------
CREATE TABLE practice_record (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES "user" (id),
    sentence_id       TEXT NOT NULL,
    step             TEXT NOT NULL CHECK (step IN ('blind_listen', 'read_along', 'shadowing',
                                                   'roleplay', 'record', 'retell', 'qa', 'substitution')),
    device_id         TEXT,                          -- 三端产生时带设备号便于人工核对
    client_created_at TIMESTAMPTZ NOT NULL,          -- 客户端时间，用于排序
    payload           JSONB,                         -- 如 {"self_rating":0.6} / {"marked":true}
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_practice_sentence ON practice_record (user_id, sentence_id, client_created_at);

-- ---------- 用户录音（默认上传云端，FR-6.3 / FR-9.7）----------
CREATE TABLE recording (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES "user" (id),
    sentence_id       TEXT NOT NULL,
    lesson_id         TEXT NOT NULL,
    step              TEXT,                          -- 录音发生的步骤（跟读/角色扮演/复述/问答）
    duration_ms       INT,
    local_path        TEXT,                         -- 客户端本地路径（离线录音的临时落点）
    storage_url       TEXT,                         -- 云端地址：与音频同一存储层
    device_id         TEXT,
    client_created_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_recording_sentence ON recording (user_id, sentence_id);

-- ---------- 复习项（句子级，一句一个，FR-7）----------
CREATE TABLE review_item (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES "user" (id),
    sentence_id  TEXT NOT NULL UNIQUE,              -- 一句一个复习项
    source       TEXT NOT NULL CHECK (source IN ('marked', 'blind_low', 'retell_stuck', 'manual', 'score_low')),
    interval_days INT NOT NULL DEFAULT 1,           -- 初始间隔 1 天（§8.5）
    error_count  INT NOT NULL DEFAULT 0,
    due_date     DATE NOT NULL,
    last_result  TEXT CHECK (last_result IN ('pass', 'fail')),
    mastered     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_review_due ON review_item (user_id, due_date) WHERE NOT mastered;

-- ---------- 词 ↔ 复习项关联（词级复习由句子承载，FR-7.2；结构待 M3 细化）----------
CREATE TABLE word_review_link (
    word_id        TEXT NOT NULL REFERENCES word (id),
    review_item_id TEXT NOT NULL REFERENCES review_item (id),
    PRIMARY KEY (word_id, review_item_id)
);

-- ---------- 即问即答与造句任务 ----------
CREATE TABLE qa_item (
    id                TEXT PRIMARY KEY,
    lesson_id         TEXT NOT NULL REFERENCES daily_lesson (id),
    type              TEXT NOT NULL CHECK (type IN ('qa', 'substitution')),
    prompt            TEXT NOT NULL,
    prompt_kana       TEXT,
    prompt_translation TEXT,
    answer            TEXT NOT NULL,                 -- 参考答案（同时作为 TTS 句子入 sentence 表）
    answer_kana       TEXT,
    sort_order        INT NOT NULL DEFAULT 0
);
CREATE INDEX idx_qa_lesson ON qa_item (lesson_id, type, sort_order);

-- ---------- 生成任务（BYOK 后 MVP 不走队列；表结构保留供「官方代生成」复用，§8.1）----------
CREATE TABLE generation_job (
    id         TEXT PRIMARY KEY,
    lesson_id  TEXT NOT NULL REFERENCES daily_lesson (id),
    type       TEXT NOT NULL CHECK (type IN ('material', 'tts', 'tts_single')),
    status     TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'partial')),
    progress   INT,                                  -- tts: 已完成句数/总句数
    error      TEXT,
    result_ref TEXT,
    attempts   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_job_lesson ON generation_job (lesson_id, status);

-- ---------- 设备同步游标（FR-9.6 拉取式同步）----------
CREATE TABLE sync_cursor (
    user_id      TEXT NOT NULL REFERENCES "user" (id),
    device_id    TEXT NOT NULL,
    last_event_id TEXT,                              -- 拉到哪了（ULID 时间有序）
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id)
);

-- ---------- 成本观测（部署文档 §9.3：一条 SQL 出月度账单）----------
CREATE TABLE llm_call_log (
    id          TEXT PRIMARY KEY,
    user_id     TEXT,
    lesson_id   TEXT,
    model       TEXT,
    prompt_tokens  INT,
    output_tokens  INT,
    duration_ms INT,
    cost_cents  NUMERIC(10, 4),                      -- 折算分（人民币）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tts_call_log (
    id          TEXT PRIMARY KEY,
    user_id     TEXT,
    sentence_id TEXT,
    voice       TEXT,
    chars       INT,                                 -- 字符数（Azure 计费口径）
    duration_ms INT,                                 -- 请求耗时
    cache_hit   BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
