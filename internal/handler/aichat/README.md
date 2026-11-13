# ai_chat 接口（aichat 包）

本目录实现 WeKnora 的自定义 AI 聊天接口 `POST /api/v1/ai/chat`（OPLink 协议，SSE 流式响应）。

## 设计定位

`ai_chat` 是一层**协议适配器**：把前端发来的、带版本号的请求协议，转换成对底层
WeKnora Agent 引擎（`sessionService.AgentQA`）或结构化搜索草稿生成的调用，并把结果
翻译成协议约定的 SSE 事件回给前端。

核心流程：**意图分析 → 注册表分发**。

- `search` 意图 → 结构化搜索流程（生成可编辑的搜索条件草稿，`search_draft`）。
- `approval` 意图 → 签审草案流程（生成可编辑的签审草案，`approval_draft`，仅 processApproval 页）。
- `approval_search` 意图 → 签核任务筛选流程（按条件从 `approvalTaskIndex` 中筛出任务绑定，`approval_task_selection`，仅 processApproval 页）。
- `normal` 意图 → Agent 流式问答（`AgentQA`，ReAct 引擎）。

## 目录结构

| 文件 | 职责 |
|---|---|
| `handler.go` | `AIChatHandler` 结构体、`NewAIChatHandler` 构造函数、`Chat` 入口分发；智能体解析、会话创建、SSE 输出工具 |
| `intent.go` | 意图注册表：`Intent` 类型、`intentHandler` 接口、`intentRegistry`；意图分类 `classifyIntent`、分发 `handleMessage`、搜索/签核/任务筛选开关 `shouldSearch`/`shouldApprove`/`shouldApprovalSearch`、落库 `persistIntent` |
| `search.go` | 搜索意图两阶段流程：`handleSearchMessage` / `handleSearchContext`、条件生成与硬校验、搜索提示词、JSON 解析、语言检测 |
| `validation.go` | 搜索条件的机器规则硬校验（字段/操作符/枚举值） |
| `agent_turn.go` | 普通问答意图：`handleAgentTurn` + `aiStreamTranslator`（事件总线 → SSE 翻译）、元数据/上下文组装 |
| `approval.go` | 签核意图：`handleApprovalMessage` + `approvalDraftTranslator`（模型输出解析为「正文+草案」envelope，结构卡控后合并为单条 `approval_draft` 事件；draftId/revision/status 由代码组装） |
| `approval_search.go` | 签核任务筛选意图：解析 `approvalTaskIndex`，调用 approval_search 智能体筛选任务，绑定校验后合并为单条 `approval_task_selection` 事件（`selectedTasks` 只含 activityId + assignmentId） |
| `approval_types.go` | 流程签审协议类型：itemData 五数组结构、签审配置/清单/草案表单、任务卡片绑定解析 |
| `approval_validation.go` | 签核机器规则：结构卡控（绑定/pathId/勾选集合）、保存与完成条件判定（§6.2/§6.3，含保存默认动作） |
| `approval_context.go` | processApproval 动态字段同步：`approvalTaskIndex`/`itemData`/`additionalItems` 的整体替换/省略沿用/`[]` 清空语义，落库为 `role="approval_context"` 消息 |
| `attachments.go` | 消息内联附件：图片/文件解码、配额与类型校验、落存储、VLM 降级描述 |
| `artifact_reference.go` | 沙箱产物引用处理（session 包同名文件的私有镜像，需保持同步）：`sandbox:` 引用重写、引用解析、合并/去重、公开视图 |
| `start.go` | `start` 阶段：建立会话、持久化基础数据、生成推荐问题 |
| `types.go` | 协议类型与常量（请求体、SSE 事件、`AIRequestType*`、`AIEvent*`、`AIProtocolVersion`） |
| `APPROVAL_PROMPT.md` | 签核智能体系统提示词全文与意图提示词增补文案（部署指引） |
| `APPROVAL_SEARCH_PROMPT.md` | 签核任务筛选智能体系统提示词全文与意图智能体提示词全文（部署指引） |

## 请求协议与意图分发

请求体由 `type` 字段区分阶段：

```
Chat (handler.go)
 ├─ "start"           → handleStart            (start.go)   建立会话上下文 + 推荐问题
 ├─ "message"         → handleMessage          (intent.go)  意图分析 + 注册表分发
 │                        └─ classifyIntent → 类型化 Intent
 │                        └─ shouldSearch / shouldApprove 开关判断
 │                        └─ intents.resolve(intent).Handle(...)
 ├─ "search_context"  → handleSearchContext    (search.go)  搜索流程第二阶段（前端回传搜索字段）
 └─ 其他               → 错误事件
```

意图分发：

```
handleMessage
 ├─ classifyIntent      用 intent_agent_id 的模型对消息分类，输出 Intent
 │                      （approval / approval_search 需意图智能体提示词含对应输出类别，
 │                       见 APPROVAL_PROMPT.md / APPROVAL_SEARCH_PROMPT.md）
 ├─ shouldSearch        search 意图仅在「search_enabled=true 且页面为 largeTable」时保留
 ├─ shouldApprove       approval 意图仅在「approval_enabled=true 且页面为 processApproval」时保留
 ├─ shouldApprovalSearch approval_search 意图仅在「approval_search_enabled=true 且页面为 processApproval」时保留
 ├─ persistIntent       把意图作为 role="intent" 消息落库（不污染 LLM 上下文）
 └─ intents.resolve     命中 search → 结构化搜索；命中 approval → 签审草案；
                        命中 approval_search → 签核任务筛选；命中/回退 normal → Agent 问答
```

## 搜索开关（search_enabled）

`search` 意图是否走结构化搜索流程，由两个条件共同决定（`intent.go` 的 `shouldSearch`）：

1. `config.yaml` 的 `ai_chat.search_enabled` 为 `true`；
2. 页面类型为 `largeTable`。

只有同时满足才走结构化搜索（生成 `search_draft` / `need_clarification` 搜索条件草稿）；
否则回退 `normal` 流式问答。例如 `singleData` 场景（答案就在 `itemdata` 里）会回退 normal，
由 Agent 直接基于数据流式回答。

## 签核开关（approval_enabled）与签审草案流程

`approval` 意图走签审草案流程，由两个条件共同决定（`intent.go` 的 `shouldApprove`）：

1. `config.yaml` 的 `ai_chat.approval_enabled` 为 `true`；
2. 页面类型为 `processApproval`（流程签审页）。

流程与搜索草稿同构（`approval.go`）：

- 请求携带 `approvalTaskIndex`（扁平任务索引）与 `itemData`（五数组：masterItem /
  approvalConfig / approvalItem / additionalItems / approvalDrafts）；顶层 `additionalItems`
  （额外资讯）同样参与同步。各字段按
  「出现整体替换、省略沿用、`[]` 清空」语义落库为 `role="approval_context"` 消息
  （`approval_context.go`），跨请求与重启后仍可回读；normal 流程在 processApproval
  会话同样回填最新值并注入元数据。
- approval 智能体经 AgentQA 执行，最终答案解析为 `{"message", "drafts"}` JSON：
  正文说明 + 0..N 份草案（每份按活动+指派绑定一个已添加任务，携带完整 form）。
  `draftId`/`revision`/`status` 由后端逐笔组装（各任务的 editing 草案优化沿用
  draftId 且 revision+1，新任务新实例 revision 1），模型只产出正文与 form。
- 结构卡控（`approval_validation.go`）：绑定必须匹配某个已添加任务（活动+指派，
  同一任务最多一份草案）、pathId ∈ 该任务 paths、checkedTaskIds ⊆ 该任务 tasks
  无重复；违约先纠正重试一次，仍违约以 `error` 事件
  （`code: APPROVAL_DRAFT_ERROR`）终止。
- 每份草案携带 `AIassessment`（`recommendation` / `summary` / `reasons`），由
  模型基于当前任务数据生成，供前端展示；不参与保存/完成判定，后端只校验三个
  字段非空，缺字段触发一次纠正重试。
- 终止事件为单条 `approval_draft`（`message` 承载正文、`approvalDrafts` 承载草案、
  `suggestions` 携带本轮推荐问题），其后不追加 `answer_done`；`drafts` 为空表示
  纯正文回答（普通咨询/澄清）。消息省略 `page` 时，意图分类与门控回退最近一次
  start 同步的页面。
- 保存/完成条件（文档 3 §6.2/§6.3）由 `computeApprovalReadiness` 逐笔确定性
  判定并注入提示词元数据（`inputFormReadiness` 数组，每条携带活动+指派绑定），
  正文措辞由提示词决定。
- 系统提示词与意图提示词增补文案见 `APPROVAL_PROMPT.md`。

## 签核任务筛选（approval_search）与 approval_task_selection 事件

`approval_search` 意图处理 processApproval 页面上的「查找/列出/筛选签核任务」请求
（例如「帮我找出这个月的未签审任务」「找出所有ECO任务」「帮我签所有ECO」——
最后一种只负责先找出任务，不签核）。是否走结构化流程由两个条件共同决定
（`intent.go` 的 `shouldApprovalSearch`）：

1. `config.yaml` 的 `ai_chat.approval_search_enabled` 为 `true`；
2. 页面类型为 `processApproval`（流程签审页）。

流程（`approval_search.go`）：

- 请求携带 `approvalTaskIndex`（扁平任务索引）与 `itemData`；缺失时按
  `approval_context` 同步值回填（顶层 `additionalItems` 额外资讯同样回填并注入
  筛选上下文）。筛选范围以索引为准；`itemData` 不注入筛选
  上下文（其完整业务物件可能很大），只用于会话历史与推荐问题。
- 与 normal / search 草稿流一样经 AgentQA 执行：思考、工具调用、工具结果等
  进度事件实时流式输出，最终答案仅在本地累积；完成时再解析/校验并发出单条
  `approval_task_selection`。
- 专用 approval_search 智能体（`ai_chat.approval_search_agent_id`）在索引中
  按时间/类型/状态/单号/处理人等条件筛选；未指定状态时默认「未签审」。
  智能体最终答案只返回 `activityId` + `assignmentId` 绑定。
- 后端校验绑定（重复去重、索引中不存在则纠正重试），只下发模型筛出的绑定；
  仍违约以 `error` 事件（`code: APPROVAL_TASK_ERROR`）终止。
- 终止事件为单条 `approval_task_selection`（`message` 承载正文说明、
  `selectedTasks` 承载 activityId + assignmentId 绑定、`suggestions` 携带本轮
  推荐问题），其后不追加 `answer_done`。索引为空时返回空列表 + 说明，不调用
  模型。
- 该流程不生成草案、不执行签核；用户确认任务后仍需走 approval 意图生成签审
  草案，再由前端执行业务接口。
- 智能体提示词与意图提示词全文见 `APPROVAL_SEARCH_PROMPT.md`。

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
- **管理员信息事件（`admin_info`）**：智能体调用 `record_admin_info` 工具记录的内部细节
  （SQL、文档 ID、系统字段名等）翻译成专属的 `admin_info` 事件下发：`content` 为记录文本，
  `data` 携带 `category`（分类，缺省 `notes`）与 `tool_call_id`。该工具**不再**产生通用
  `tool_call`/`tool_result` 帧，前端按 `category` 分组渲染管理员折叠面板即可，无需匹配
  工具名。工具的启用（智能体 `allowed_tools`）与文案/枚举（config.yaml `admin_info_tool`）
  见 `internal/agent/tools/record_admin_info.go`。
- **沙箱产物（artifacts）**：normal 意图回合结束后，`aiStreamTranslator` 与官方
  `/agent-chat` 完成路径同源地回收沙箱输出目录（`ArtifactCollector`），把答案里的
  `sandbox:<文件名>` 占位重写为稳定 `resource://` 句柄，产物列表随助手消息落库，
  并在 `answer_done` 之后、`suggestions` 之前推送一个 `artifacts` 事件。事件
  `data` 携带 `artifacts`（含 `file_name`/`file_type`/`file_size`/`handle`/
  `download_path`，下载走 `/api/v1/sessions/{session_id}/messages/{message_id}/artifacts/{index}/download`，
  鉴权与本接口一致）、`final_content`（重写后的完整答案，可替换已流式的占位文本）、
  `session_id` 与 `assistant_message_id`。本轮无产物时不推送该事件。
- **上下文流转**：`start` 阶段把 `page` / `searchFields` / `searchBody` / `itemdata` / `additionalItems`
  持久化为 `role="start"` 消息；`message` / `search_context` 阶段通过 `loadStartContext` 回填。
  `itemdata` 不限于 `singleData` 页面（`largeTable` 也可携带表格数据），normal 问答与搜索
  草稿流程都会把它注入智能体上下文。
- **额外资讯（additionalItems）**：请求顶层与 `itemdata` 同级的补充数据，不限制数组还是
  JSON 对象，内容结构由前端决定、由 AI 自行分析。它是把补充资讯从 `itemdata` 中拆出来的
  独立通道：normal / search / approval / approval_search 四条分派路径都会把它注入对应智能体
  的结构化上下文（approval_search 的 itemData 因体量原因不注入，但 additionalItems 是前端
  主动挑选的载荷，照常注入），并参与推荐问题生成；processApproval 会话同样按
  「出现整体替换、省略沿用、`[]` 清空」语义走 `approval_context` 同步通道。不携带该字段时
  各流程行为与只发 `itemdata` 的历史方式完全一致。注意与 processApproval 页 itemData 五数组
  结构里的内层 `additionalItems` 子键无关。
- **搜索两阶段**：`message` 识别为搜索意图、但 `start` 未提供 `searchFields` 时，返回
  `need_search_context` 并写入内存态 `h.actions`（`pendingAction`），等前端用 `search_context`
  阶段回传字段后再生成草稿。**该状态仅存内存，重启丢失。**
- **结构化载荷落库（`ai_chat_payload`）**：`search_draft` / `approval_draft` /
  `approval_task_selection` 三种终止事件在成功发出的同时，把与事件同构的载荷（`intent` +
  `eventType` + `conditions`/`searchBody` / `approvalDrafts` / `selectedTasks` / `suggestions`）
  随助手消息写入 messages 表的 `ai_chat_payload` JSONB 列（迁移 `000137` / sqlite `000053`）。
  官方历史接口 `GET /api/v1/messages/{session_id}/load` 因此能返回结构化 JSON，前端刷新恢复
  会话时按 `eventType` 判别即可复用实时流的渲染逻辑。注意：该列**不进入 LLM 上下文**
  （历史构建器只读 Content），也不写入 Content——approval / approval_search 的 Content 仍只存
  正文纯文本（防止 normal 意图模仿 JSON），search 意图的 Content 仍存 envelope JSON（refinement
  依赖）。契约违约与错误轮次不写载荷；`selectedTasks` 只含 activityId + assignmentId，回放时
  找不到对应任务需容错。载荷类型与序列化见 `types.go` 的 `AIChatEventPayload`。
- **模型优先级**：`ai_chat.model_id`（配置）> 智能体自身 `model_id`，见 `applyModelOverride`。
- **主智能体解析顺序**：`ai_chat.agent` 内联定义（config.yaml 直接定义，无需建库）
  → `ai_chat.agent_id`（custom_agents 表）→ 环境变量 `WEKNORA_AI_AGENT_ID`
  → 内置默认值 `defaultAIAgentID`（`handler.go`）。

## 内联定义主智能体（ai_chat.agent）

`ai_chat.agent` 段可直接在 config.yaml 中定义 normal 意图使用的主智能体，字段即完整的
`CustomAgentConfig`（与智能体编辑器一致），未写的字段由 `EnsureDefaults` 补默认值。
存在即优先于 `agent_id`，不查 `custom_agents` 表：

```yaml
ai_chat:
  agent:
    id: "ai-chat-main"              # 可选；留空用合成 ID "ai-chat-inline-agent"
    name: "AI 问答主智能体"
    agent_mode: "smart-reasoning"
    system_prompt_id: "default_kb"  # 引用 prompt_templates，或直接写 system_prompt
    model_id: "..."                 # 须为 models 表中有效的问答模型
    kb_selection_mode: "none"
    allowed_tools: ["knowledge_search"]
```

注意：

- `system_prompt_id` / `context_template_id` 在启动时对照 `prompt_templates/` 解析（同内置智能体）。
- `ai_chat.model_id` 覆盖对内联定义同样生效。
- 助手消息落库时 `agent_id` 记内联 `id`（或合成 ID），`agent_tenant_id` 记请求租户。
- `intent_agent_id` / `search_agent_id` / `recommend_agent_id` 仍指向 DB 智能体；未配置时
  回退主智能体，即继承内联定义。

## 相关配置

`config/config.yaml` 的 `ai_chat` 段：

```yaml
ai_chat:
  agent:               # 内联定义主智能体（可选）；存在则忽略 agent_id
    agent_mode: "smart-reasoning"
    system_prompt_id:  "..."
    model_id:          "..."
  agent_id:            "..."   # 主智能体（DB）；agent 段存在时被忽略
  model_id:            "..."   # 覆盖智能体自身对话模型
  intent_agent_id:     "..."   # 意图分类智能体
  recommend_agent_id:  "..."   # 推荐问题智能体
  search_agent_id:     "..."   # 搜索条件生成智能体
  search_enabled:      true    # 搜索开关：true 时 largeTable 走结构化搜索，false 全走流式问答
  approval_agent_id:   "..."   # 签核草案生成智能体（DB）；approval 段存在时被忽略
  approval:                    # 内联定义签核智能体（可选），提示词见 APPROVAL_PROMPT.md
    system_prompt:     "..."
  approval_enabled:    true    # 签核开关：true 时 processApproval 走签审草案，false 全走流式问答
  approval_search_agent_id: "..." # 签核任务筛选智能体（DB），提示词见 APPROVAL_SEARCH_PROMPT.md
  approval_search_enabled:  true  # 任务筛选开关：true 时 processApproval 的任务查找返回 approval_task_selection
```
