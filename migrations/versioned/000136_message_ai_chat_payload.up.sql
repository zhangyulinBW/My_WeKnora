-- ai_chat 终止事件的结构化载荷（search_draft / approval_draft /
-- approval_task_selection）：随助手消息落库，供官方历史接口
-- GET /messages/{session_id}/load 回放结构化卡片。
-- 该列不进入 LLM 上下文（历史构建器只读 Content）。

ALTER TABLE messages ADD COLUMN IF NOT EXISTS ai_chat_payload JSONB;
