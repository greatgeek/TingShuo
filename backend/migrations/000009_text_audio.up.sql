-- 任意文本 TTS 缓存：生词本例句等不在 sentence 表的文本，按 (text,voice,rate) 哈希缓存 MP3。
BEGIN;

CREATE TABLE text_audio (
    cache_key  TEXT PRIMARY KEY,   -- sha256(text|voice|rate)
    voice      TEXT NOT NULL,
    rate       REAL NOT NULL DEFAULT 1.0,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
