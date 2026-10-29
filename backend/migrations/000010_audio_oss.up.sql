-- 音频统一存 OSS：word_audio / text_audio 不再存字节，只留元信息（行存在=OSS 有对象）。
-- 对象 key：word-<word_id>.mp3 / say-<cache_key>.mp3（与句子音频的 64hex.mp3 不冲突）。
-- 既有 DB 字节缓存丢弃（可再生成，成本极低）。
BEGIN;

ALTER TABLE word_audio DROP COLUMN IF EXISTS data;
ALTER TABLE text_audio DROP COLUMN IF EXISTS data;

COMMIT;
