-- 课文本：原声课文（练口语整段音频）+ SRT 时间轴 + 逐行背诵/听写进度。
-- 内容是全局共享的（不随用户变化），进度按用户存。
CREATE TABLE recitation_lesson (
    id          TEXT PRIMARY KEY,
    lesson_no   INT NOT NULL,
    title       TEXT NOT NULL,
    audio_key   TEXT NOT NULL,               -- 对象存储 key（不含 .mp3 后缀）
    duration_ms INT NOT NULL,
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE recitation_line (
    id          TEXT PRIMARY KEY,
    lesson_id   TEXT NOT NULL REFERENCES recitation_lesson (id) ON DELETE CASCADE,
    line_no     INT NOT NULL,
    start_ms    INT NOT NULL,
    end_ms      INT NOT NULL,
    text        TEXT NOT NULL,
    translation TEXT NOT NULL DEFAULT '',
    ruby_tokens JSONB,
    UNIQUE (lesson_id, line_no)
);
CREATE INDEX idx_recitation_line_lesson ON recitation_line (lesson_id, line_no);

CREATE TABLE recitation_progress (
    user_id         TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    lesson_id       TEXT NOT NULL REFERENCES recitation_lesson (id) ON DELETE CASCADE,
    line_no         INT NOT NULL,
    recited_at      TIMESTAMPTZ,             -- 背诵自评「会背了」
    dict_ok_at      TIMESTAMPTZ,             -- 听写以汉字写法完全正确
    dict_reading_at TIMESTAMPTZ,             -- 听写以假名读音正确（含混合写法）
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, lesson_id, line_no)
);
