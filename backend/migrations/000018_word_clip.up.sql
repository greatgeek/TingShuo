-- 生词原声例句：从 Nadeshiko 日剧/动画语料里按词精准选出的短片段（含视频/音频/截图）。
-- 缓存键 word_key = 词面|读音，跨用户共享：同一词只花一次 API 配额，所有用户复用。
-- 片段只做展示与点播，CDN 直链不进数据库流量；配额消耗只发生在搜索阶段。

CREATE FUNCTION word_clip_key(surface text, reading text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT btrim(surface) || '|' || btrim(COALESCE(reading, ''))
$$;

CREATE TABLE word_clip (
    word_key        TEXT NOT NULL,
    segment_id      TEXT NOT NULL,           -- Nadeshiko segment publicId
    text_ja         TEXT NOT NULL,
    text_zh         TEXT,                    -- LLM 中文翻译；失败时回退英文
    text_en         TEXT,
    zh_source       TEXT NOT NULL DEFAULT '', -- llm / en / ''
    tokens          JSONB NOT NULL DEFAULT '[]'::jsonb,
    match_start     INTEGER NOT NULL DEFAULT 0, -- 目标词在 text_ja 中的 rune 起止
    match_end       INTEGER NOT NULL DEFAULT 0,
    match_text      TEXT NOT NULL DEFAULT '',
    start_ms        INTEGER NOT NULL,
    end_ms          INTEGER NOT NULL,
    media_id        TEXT NOT NULL DEFAULT '',
    media_name      TEXT NOT NULL DEFAULT '',
    episode         INTEGER NOT NULL DEFAULT 0,
    episode_title   TEXT NOT NULL DEFAULT '',
    image_url       TEXT NOT NULL DEFAULT '',
    audio_url       TEXT NOT NULL DEFAULT '',
    video_url       TEXT NOT NULL DEFAULT '',
    score           REAL NOT NULL DEFAULT 0,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (word_key, segment_id)
);
CREATE INDEX word_clip_word_key_idx ON word_clip (word_key, sort_order);

CREATE TABLE word_clip_queue (
    word_key    TEXT PRIMARY KEY,
    surface     TEXT NOT NULL,
    reading     TEXT NOT NULL DEFAULT '',
    meaning     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending', -- pending/running/done/skipped/failed
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX word_clip_queue_status_idx ON word_clip_queue (status, created_at);
