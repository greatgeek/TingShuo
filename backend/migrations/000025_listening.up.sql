-- 听力训练：真人听力素材按大题切段，录音后作答判断对错/选择题，判分后揭晓原文。
-- 内容全局共享，作答进度按 (user, lesson, section) 存。
CREATE TABLE listening_lesson (
    id          TEXT PRIMARY KEY,
    lesson_no   INT NOT NULL UNIQUE,
    title       TEXT NOT NULL,
    audio_key   TEXT NOT NULL,               -- /media 下的相对路径，或完整 URL
    duration_ms INT NOT NULL,
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE listening_section (
    id             TEXT PRIMARY KEY,
    lesson_id      TEXT NOT NULL REFERENCES listening_lesson (id) ON DELETE CASCADE,
    section_no     INT NOT NULL,
    kind           TEXT NOT NULL,            -- tf=判断对错；choice=选择
    instruction    TEXT NOT NULL DEFAULT '',
    audio_start_ms INT NOT NULL,
    audio_end_ms   INT NOT NULL,
    example_prompt TEXT NOT NULL DEFAULT '', -- 例题（讲义中的「例」）
    example_answer TEXT NOT NULL DEFAULT '',
    UNIQUE (lesson_id, section_no)
);

CREATE TABLE listening_question (
    id          TEXT PRIMARY KEY,
    section_id  TEXT NOT NULL REFERENCES listening_section (id) ON DELETE CASCADE,
    question_no INT NOT NULL,
    prompt      TEXT NOT NULL,
    options     JSONB NOT NULL DEFAULT '[]'::jsonb,  -- [{key,text}]；tf 为空
    answer      TEXT NOT NULL,
    UNIQUE (section_id, question_no)
);

CREATE TABLE listening_line (
    id          TEXT PRIMARY KEY,
    section_id  TEXT NOT NULL REFERENCES listening_section (id) ON DELETE CASCADE,
    line_no     INT NOT NULL,
    speaker     TEXT NOT NULL DEFAULT '',
    start_ms    INT NOT NULL,
    end_ms      INT NOT NULL,
    text        TEXT NOT NULL,
    translation TEXT NOT NULL DEFAULT '',
    ruby_tokens JSONB,
    UNIQUE (section_id, line_no)
);

CREATE TABLE listening_progress (
    user_id    TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    lesson_id  TEXT NOT NULL REFERENCES listening_lesson (id) ON DELETE CASCADE,
    section_no INT NOT NULL,
    answers    JSONB NOT NULL DEFAULT '{}'::jsonb,  -- {"1":"○","2":"b"}
    score      INT NOT NULL DEFAULT 0,
    total      INT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, lesson_id, section_no)
);