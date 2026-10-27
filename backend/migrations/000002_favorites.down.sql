-- 回滚收藏表
BEGIN;
DROP TABLE IF EXISTS favorite_material;
DROP TABLE IF EXISTS favorite_sentence;
COMMIT;
