-- 播客集：v13 单场双语播客的落库与缓存（同脚本零重复计费）
CREATE TABLE podcast_episode (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES "user" (id),
    lesson_id   TEXT REFERENCES daily_lesson (id),   -- 可空：允许脱离课程的自由脚本
    cache_key   TEXT NOT NULL UNIQUE,                -- sha256(lesson+脚本+引擎版本)
    script      JSONB NOT NULL,                      -- [{role,text}]
    timeline    JSONB NOT NULL,                      -- [{role,text,start_ms,end_ms}]
    srt         TEXT NOT NULL DEFAULT '',
    storage_key TEXT NOT NULL,                       -- OSS 对象 key
    duration_ms INT NOT NULL,
    rounds      INT NOT NULL,
    llm_model   TEXT,
    tts_engine  TEXT NOT NULL DEFAULT 'doubao-podcast-v13',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_podcast_user_lesson ON podcast_episode (user_id, lesson_id, created_at DESC);
