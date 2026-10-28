-- generation_job 落库（部署文档 §4）：LLM 生成任务跨重启可查。
-- output 存累计流式文本（轮询与重启后取回用）；重启时残留 running 由启动清理置 failed。
BEGIN;

ALTER TABLE generation_job ADD COLUMN IF NOT EXISTS output TEXT;

COMMIT;
