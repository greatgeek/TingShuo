-- 单词发音缓存：一词一音频（默认音色/语速），首取时 Azure 合成后落库，
-- 之后直接读库返回（零 Azure 调用）。单词 MP3 很小（约 10—30KB），存库即可。
BEGIN;

CREATE TABLE word_audio (
    word_id    TEXT PRIMARY KEY REFERENCES word (id),
    voice      TEXT NOT NULL,
    rate       REAL NOT NULL DEFAULT 1.0,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
