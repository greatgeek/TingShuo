-- 问AI 对话留痕：按 用户+句子 保存完整聊天记录（跨设备可见）
CREATE TABLE ai_chat (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    sentence_text TEXT NOT NULL,
    role         TEXT NOT NULL,              -- user | assistant
    content      TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ai_chat_user_sent ON ai_chat (user_id, sentence_text, created_at);
