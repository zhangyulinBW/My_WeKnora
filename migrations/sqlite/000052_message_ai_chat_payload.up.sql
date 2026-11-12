-- Mirrors versioned migration 000136_message_ai_chat_payload:
-- ai_chat 终止事件的结构化载荷随助手消息落库。

ALTER TABLE messages ADD COLUMN ai_chat_payload TEXT;
