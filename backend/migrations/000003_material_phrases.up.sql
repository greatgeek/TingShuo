-- 材料类型扩展：phrases（生词短语，FR-2.15 搭配卡的落地形态）
-- 每个生词一个自然短语句子，复用 sentence/audio_asset/收藏 全套句子级管线
BEGIN;
ALTER TABLE material DROP CONSTRAINT material_type_check;
ALTER TABLE material ADD CONSTRAINT material_type_check
    CHECK (type IN ('dialogue', 'passage', 'phrases'));
COMMIT;
