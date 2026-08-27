# ai_chat 接口（aichat 包）

本目录实现 WeKnora 的自定义 AI 聊天接口 `POST /api/v1/ai/chat`（OPLink 协议，SSE 流式响应）。

## 设计定位

`ai_chat` 是一层**协议适配器**：把前端发来的、带版本号的请求协议，转换成对底层
WeKnora Agent 引擎（`sessionService.AgentQA`）或结构化搜索草稿生成的调用，并把结果
翻译成协议约定的 SSE 事件回给前端。

核心流程：**意图分析 → 注册表分发**。

- `search` 意图 → 结构化搜索流程（生成可编辑的搜索条件草稿，`search_draft`）。
- `normal` 意图 → Agent 流式问答（`AgentQA`，ReAct 引擎）。

## 目录结构

| 文件 | 职责 |
|---|---|
| `handler.go` | `AIChatHandler` 结构体、`NewAIChatHandler` 构造函数、`Chat` 入口分发；智能体解析、会话创建、SSE 输出工具 |
| `intent.go` | 意图注册表：`Intent` 类型、`intentHandler` 接口、`intentRegistry`；意图分类 `classifyIntent`、分发 `handleMessage`、搜索开关 `shouldSearch`、落库 `persistIntent` |
| `search.go` | 搜索意图两阶段流程：`handleSearchMessage` / `handleSearchContext`、条件生成与硬校验、搜索提示词、JSON 解析、语言检测 |
| `validation.go` | 搜索条件的机器规则硬校验（字段/操作符/枚举值） |
| `agent_turn.go` | 普通问答意图：`handleAgentTurn` + `aiStreamTranslator`（事件总线 → SSE 翻译）、元数据/上下文组装 |
| `start.go` | `start` 阶段：建立会话、持久化基础数据、生成推荐问题 |
| `types.go` | 协议类型与常量（请求体、SSE 事件、`AIRequestType*`、`AIEvent*`、`AIProtocolVersion`） |

## 请求协议与意图分发

请求体由 `type` 字段区分阶段：

```
Chat (handler.go)
 ├─ "start"           → handleStart            (start.go)   建立会话上下文 + 推荐问题
 ├─ "message"         → handleMessage          (intent.go)  意图分析 + 注册表分发
 │                        └─ classifyIntent → 类型化 Intent
 │                        └─ shouldSearch 开关判断
 │                        └─ intents.resolve(intent).Handle(...)
 ├─ "search_context"  → handleSearchContext    (search.go)  搜索流程第二阶段（前端回传搜索字段）
 └─ 其他               → 错误事件
```

意图分发：

```
handleMessage
 ├─ classifyIntent   用 intent_agent_id 的模型对消息分类，输出 Intent
 ├─ shouldSearch     search 意图仅在「search_enabled=true 且页面为 largeTable」时保留
 ├─ persistIntent    把意图作为 role="intent" 消息落库（不污染 LLM 上下文）
 └─ intents.resolve  命中 search → 结构化搜索；命中/回退 normal → Agent 问答
```

## 搜索开关（search_enabled）

`search` 意图是否走结构化搜索流程，由两个条件共同决定（`intent.go` 的 `shouldSearch`）：

1. `config.yaml` 的 `ai_chat.search_enabled` 为 `true`；
2. 页面类型为 `largeTable`。

只有同时满足才走结构化搜索（生成 `search_draft` / `need_clarification` 搜索条件草稿）；
否则回退 `normal` 流式问答。例如 `singleData` 场景（答案就在 `itemdata` 里）会回退 normal，
由 Agent 直接基于数据流式回答。

## 如何新增一种意图

1. 新增常量（`intent.go`）：

   ```go
   const IntentAnalyze Intent = "analyze"
   ```

2. 实现一个 `intentHandler`（`intent.go`，也可新建文件）：

   ```go
   type analyzeIntentHandler struct{ h *AIChatHandler }
   func (a *analyzeIntentHandler) Intent() Intent { return IntentAnalyze }
   func (a *analyzeIntentHandler) Handle(...) { /* 处理逻辑 */ }
   ```

3. 在 `NewAIChatHandler` 里注册（`handler.go`）：

   ```go
   h.intents.register(&analyzeIntentHandler{h: h})
   ```

> 注意：意图字符串值会被持久化到消息表的 `role="intent"` 消息里，因此**一旦上线，
> 已有意图的字符串值不要改名**，否则历史会话里的意图记录会失效。

## 关键行为说明

- **SSE 输出**：所有响应通过 `writeEvent` 以 `data: {json}\n\n` 输出（`handler.go`）。
- **上下文流转**：`start` 阶段把 `page` / `searchFields` / `searchBody` / `itemdata` 持久化为
  `role="start"` 消息；`message` / `search_context` 阶段通过 `loadStartContext` 回填。
- **搜索两阶段**：`message` 识别为搜索意图、但 `start` 未提供 `searchFields` 时，返回
  `need_search_context` 并写入内存态 `h.actions`（`pendingAction`），等前端用 `search_context`
  阶段回传字段后再生成草稿。**该状态仅存内存，重启丢失。**
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
  search_enabled:      true    # 搜索开关：true 时 largeTable 走结构化搜索，false 全走流式问答
```
