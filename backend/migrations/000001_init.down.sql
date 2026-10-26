-- 回滚初始 Schema（仅开发期使用；破坏性变更一律走「加新列→双写→切读→删旧列」，不依赖 down 回滚，见部署文档 §4）
BEGIN;
DROP TABLE IF EXISTS tts_call_log;
DROP TABLE IF EXISTS llm_call_log;
DROP TABLE IF EXISTS sync_cursor;
DROP TABLE IF EXISTS generation_job;
DROP TABLE IF EXISTS qa_item;
DROP TABLE IF EXISTS word_review_link;
DROP TABLE IF EXISTS review_item;
DROP TABLE IF EXISTS recording;
DROP TABLE IF EXISTS practice_record;
DROP TABLE IF EXISTS audio_asset;
DROP TABLE IF EXISTS sentence;
DROP TABLE IF EXISTS material;
DROP TABLE IF EXISTS lesson_word;
DROP TABLE IF EXISTS word;
DROP TABLE IF EXISTS daily_lesson;
DROP TABLE IF EXISTS "user";
COMMIT;
