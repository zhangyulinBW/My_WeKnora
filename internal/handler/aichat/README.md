# ai_chat 接口（aichat 包）

本目录实现 WeKnora 的自定义 AI 聊天接口 `POST /api/v1/ai/chat`（OPLink 协议，SSE 流式响应）。

## 设计定位

`ai_chat` 是一层**协议适配器**：它把前端发来的、带版本号的请求协议，转换成对底层
WeKnora Agent 引擎（`sessionService.AgentQA`）的调用，并把引擎的事件流翻译成协议约定的
SSE 事件回给前端。

核心抽象是**意图注册表**——请求进入后先做意图分类，再按意图分发到对应处理逻辑。
新增一种意图（例如「数据分析」「图表生成」）不需要改动分发主干，只需注册一个新 handler。

## 目录结构

| 文件 | 职责 |
|---|---|
| `handler.go` | `AIChatHandler` 结构体、`NewAIChatHandler` 构造函数、`Chat` 入口分发；智能体解析（`resolveAgent` 等）、会话创建、SSE 输出工具 |
| `intent.go` | **意图注册表**（核心）：`Intent` 类型、`intentHandler` 接口、`intentRegistry`；意图分类 `classifyIntent`、分发 `handleMessage`、落库 `persistIntent` |
| `search.go` | 搜索意图两阶段流程：`handleSearchMessage` / `handleSearchContext`、条件生成与硬校验、搜索提示词、JSON 解析、语言检测 |
| `agent_turn.go` | 普通问答意图：`handleAgentTurn` + `aiStreamTranslator`（事件总线 → SSE 翻译）、元数据/上下文组装 |
| `start.go` | `start` 阶段：建立会话、持久化基础数据、生成推荐问题 |
| `types.go` | 协议类型与常量（请求体、SSE 事件、`AIRequestType*`、`AIEvent*`、`AIProtocolVersion`）。这是前后端契约，json tag 不可随意改动 |
| `validation.go` | 搜索条件的机器规则硬校验（字段/操作符/枚举值） |

## 请求协议与意图分发

请求体由 `type` 字段区分阶段（`types.go` 中的 `AIRequestType*` 常量）：

```
Chat (handler.go)
 ├─ "start"           → handleStart            (start.go)
 ├─ "message"         → handleMessage          (intent.go)  ← 按意图二次分发
 │                        └─ classifyIntent → 类型化 Intent
 │                        └─ intents.resolve(intent).Handle(...)
 ├─ "search_context"  → handleSearchContext    (search.go)
 └─ 其他               → 错误事件
```

意图分发（`intent.go`）：

```
handleMessage
 ├─ classifyIntent   用 intent_agent_id 的模型对消息分类，输出 Intent
 ├─ persistIntent    把意图作为 role="intent" 消息落库（不污染 LLM 上下文）
 └─ intents.resolve(intent).Handle(...)   未命中 → 回退 normal 意图
```

## 意图注册表：如何新增一种意图

新增意图只需三步，无需改动 `handleMessage` 分发主干：

1. **新增意图常量**（`intent.go`）：

   ```go
   const IntentAnalyze Intent = "analyze"
   ```

2. **实现一个 `intentHandler`**（可放在 `intent.go`，也可新建文件）：

   ```go
   type analyzeIntentHandler struct{ h *AIChatHandler }

   func (a *analyzeIntentHandler) Intent() Intent { return IntentAnalyze }

   func (a *analyzeIntentHandler) Handle(
       ctx context.Context,
       c *gin.Context,
       req *AIChatRequest,
       agent *types.CustomAgent,
       tenantID uint64,
   ) {
       // 该意图的处理逻辑
   }
   ```

3. **在 `NewAIChatHandler` 里注册**（`handler.go`）：

   ```go
   h.intents.register(&analyzeIntentHandler{h: h})
   ```

分类模型只需输出意图名（如 `"analyze"`），`classifyIntent` 会把输出归一化后交给注册表
匹配；模型输出未识别的意图时，注册表回退到 `normal`（普通问答）。

> 注意：意图字符串值会被持久化到消息表的 `role="intent"` 消息里，因此**一旦上线，
> 已有意图的字符串值不要改名**，否则历史会话里的意图记录会失效。

## 关键行为说明

- **SSE 输出**：所有响应通过 `writeEvent` 以 `data: {json}\n\n` 输出（`handler.go`）。
- **搜索两阶段**：当 `message` 阶段识别为搜索意图、但 `start` 阶段未提供 `searchFields` 时，
  会返回 `need_search_context` 事件并写入内存态 `h.actions`（`search.go` 的 `pendingAction`），
  等前端用 `search_context` 阶段回传字段后再生成草稿。**该状态仅存内存，重启丢失。**
- **模型优先级**：`ai_chat.model_id`（配置）> 智能体自身 `model_id`，见 `applyModelOverride`。
- **智能体 ID 解析顺序**：`config.yaml ai_chat.agent_id` → 环境变量 `WEKNORA_AI_AGENT_ID`
  → 内置默认值 `defaultAIAgentID`（`handler.go`）。

## 相关配置

`config/config.yaml` 的 `ai_chat` 段：

```yaml
ai_chat:
  agent_id:            "..."   # 主智能体
  model_id:            "..."   # 覆盖智能体自身对话模型
  intent_agent_id:     "..."   # 意图分类智能体
  recommend_agent_id:  "..."   # 推荐问题智能体
```
