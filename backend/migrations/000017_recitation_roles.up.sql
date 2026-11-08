-- 课文本「对戏」模式：每行标注说话人（一行可能两人合说）与场景说明。
-- speakers 为空表示暂未标注，前端按普通行处理。
ALTER TABLE recitation_line ADD COLUMN speakers JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE recitation_line ADD COLUMN scene TEXT NOT NULL DEFAULT '';
