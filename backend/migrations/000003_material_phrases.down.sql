-- 回滚材料类型扩展
BEGIN;
ALTER TABLE material DROP CONSTRAINT material_type_check;
ALTER TABLE material ADD CONSTRAINT material_type_check
    CHECK (type IN ('dialogue', 'passage'));
COMMIT;
