# 腾讯 WeKnora 上手指南：功能、安装、配置与注意事项

> 本文基于腾讯官方开源库 [Tencent/WeKnora](https://github.com/Tencent/WeKnora)（MIT 协议，写作时最新版本 v0.8.0）整理，覆盖功能介绍、Docker 与 Ubuntu 两种安装方式、功能配置推荐，以及个人部署过程中的踩坑注意事项。文中配置值为笔者实际部署验证过的推荐组合，供参考。

## 一、WeKnora 是什么

如果团队里散落着大量文档，想让它们变得可搜索、可问答，WeKnora 值得一试。它是腾讯开源的大模型知识管理框架（MIT 协议），一句话概括：**开箱即用的私有化 RAG 知识库 + Agent 平台**。文档解析、向量化、检索、大模型推理这条链路全部模块化，大模型、向量数据库、对象存储都能按需替换，整套系统可以完全跑在自己的服务器或私有云里，数据不出内网。

三大核心能力：

- **RAG 快速问答**：基于知识库的检索增强问答，回答带原文引用；
- **ReAct Agent 智能推理**：自主编排知识检索、MCP 工具、技能沙箱（Docker / E2B / Cube）与网络搜索，完成多步复杂任务；
- **Wiki 模式**：Agent 从原始文档自动生成相互链接的 Markdown Wiki 与可视化知识图谱，支持人工编辑、版本历史与一键回滚。

## 二、功能概览

### 智能对话

| 能力 | 说明 |
|------|------|
| RAG 快速问答 | 基于知识库的检索增强问答，回答带原文引用与流水线进度 |
| ReAct Agent 推理 | 自主编排知识检索、MCP 工具、网络搜索，完成多步复杂任务 |
| Wiki 模式 | Agent 从原始文档自动生成相互链接的 Markdown Wiki 与可视化知识图谱，支持人工编辑、版本历史与一键回滚 |
| 技能目录与沙箱 | 技能安装到会话级 Docker / E2B / Cube 沙箱中执行 |
| 长期记忆 | 跨会话记住用户画像与偏好，自动抽取、按需检索 |
| 工具与搜索 | 内置工具、MCP 工具（含 OAuth2）、11 种网络搜索引擎 |
| 对话体验 | Prompt 在线编辑、检索阈值调节、引用浮层、推荐问题与追问、会话内临时附件 |

### 知识管理

| 能力 | 说明 |
|------|------|
| 知识库类型 | FAQ / 文档 / Wiki 三类；支持文件夹树（保留目录结构）、URL 导入、多标签管理 |
| 文档格式 | PDF / Word / Excel / PPT / 图片 / Markdown / HTML / EPUB / CSV / JSON / XMind 等十余种 |
| 数据源同步 | 飞书知识库 / 飞书云盘 / GitLab / 腾讯 IMA / Notion / 语雀 / 钉钉文档 / RSS 自动同步，支持增量与全量 |
| 检索策略 | BM25 稀疏 + Dense 稠密混合召回、GraphRAG 图谱增强、父子分块、pgvector HNSW 加速 |
| 分块管理 | 分块在线编辑、逐版本 diff 与回滚、批量重新解析、按批次自定义解析配置 |
| 质量评估 | 端到端检索 + 生成全链路测试，BLEU / ROUGE 等指标 |

### 集成与扩展

| 能力 | 说明 |
|------|------|
| 模型厂商 | OpenAI / Anthropic / DeepSeek / Qwen / 智谱 / 混元 / Gemini / Ollama / LiteLLM 等二十余家 |
| 向量数据库 | PostgreSQL(pgvector) / Elasticsearch / OpenSearch / Milvus / Weaviate / Qdrant / Apache Doris / 腾讯云 VectorDB |
| 对象存储 | 本地 / MinIO / 腾讯云 COS / AWS S3 / 阿里云 OSS 等 |
| IM 集成 | 企业微信 / 飞书 / Slack / Telegram / 钉钉 / QQBot / 微信等 10 个平台 |
| 多端接入 | Web UI / RESTful API / CLI / Chrome 插件 / 网站嵌入 Widget / 微信小程序 / 官方 MCP Server（29 个工具） |

### 平台能力

| 能力 | 说明 |
|------|------|
| 权限与安全 | 空间 RBAC 四级角色（Owner / Admin / Contributor / Viewer）+ 审计日志；凭据 AES-256-GCM 静态加密 |
| 可观测性 | Langfuse 全链路追踪：ReAct 循环、Token 消耗、任务流水线 |
| 任务管理 | MQ 异步任务队列 + 分阶段 Worker 池治理；版本升级自动数据库迁移 |
| 部署形态 | 本地 / Docker / Kubernetes (Helm)，支持私有化离线部署 |

## 三、安装部署

### 方式一：Docker Compose（推荐）

环境要求：Docker、Docker Compose、Git。

```bash
git clone https://github.com/Tencent/WeKnora.git
cd WeKnora
cp .env.example .env   # 按需编辑 .env，文件内注释很详细
docker compose pull    # 拉取最新镜像
docker compose up -d   # 启动核心服务
```

启动成功后访问 **http://localhost** 即可（后端 API 在 `:8080`）。如需使用宿主机本地 Ollama 模型，先执行 `ollama serve > /dev/null 2>&1 &`。

默认只启动核心服务，可选组件通过 `--profile` 按需叠加：

| Profile | 说明 | 命令示例 |
|---------|------|----------|
| （默认） | 核心服务 | `docker compose up -d` |
| `neo4j` | 知识图谱 | `docker compose --profile neo4j up -d` |
| `minio` | 对象存储 | `docker compose --profile minio up -d` |
| `langfuse` | 链路追踪 | `docker compose --profile langfuse up -d` |
| `full` | 全部功能 | `docker compose --profile full up -d` |

**升级**：在 `.env` 中把 `WEKNORA_VERSION` 改为目标版本后，执行 `docker compose pull && docker compose up -d`。注意只跑 `up -d` 不 `pull` 会复用本地缓存旧镜像，导致界面显示版本与预期不一致。

### 方式二：Ubuntu 源码部署（开发模式）

适合需要改代码、频繁调试的场景：后端 Go 进程 + 前端 Vite 本地运行，只有基础设施（PostgreSQL / Redis / MinIO / Neo4j / DocReader）跑在 Docker 里，改代码不用重新构建镜像。

前置条件（Ubuntu 22.04/24.04 实测可用）：

- Go ≥ 1.26（`go.mod` 要求 1.26.0）
- Node.js ≥ 20.19（前端 Vite 7 的硬性要求，建议直接装 22.x LTS）
- Docker & Docker Compose、Git、make
- 可选：Air 热重载（`go install github.com/air-verse/air@latest`）

```bash
git clone https://github.com/Tencent/WeKnora.git
cd WeKnora
cp .env.example .env

# 终端 1：启动基础设施容器
make dev-start

# 终端 2：启动后端（Air 热重载，5~10 秒生效）
make dev-app

# 终端 3：启动前端
make dev-frontend
```

访问 **http://localhost:5173**（前端开发服务器，自动代理后端 8080）。也可以用 `./scripts/dev.sh start|app|frontend` 或交互式的 `./scripts/quick-dev.sh` 达到同样效果。注意：开发模式不适合当生产环境，正式使用还是走 Docker Compose 或 Helm。

## 四、功能配置（实测推荐配置）

### 1. `.env` 关键配置项

| 配置项 | 推荐值 | 说明 |
|--------|--------|------|
| `TZ` | `Asia/Shanghai` | 时区，日志时间好对 |
| `DB_DRIVER` / `DB_HOST` | `postgres` / `postgres` | 官方默认 PostgreSQL；镜像用 ParadeDB（pgvector + BM25，一份库同时扛向量与全文检索） |
| `STREAM_MANAGER_TYPE` / `REDIS_ADDR` | `redis` / `redis:6379` | 异步任务流，必开 |
| `STORAGE_TYPE` | `local` | 入门最简；多机或需要图片外链时再上 MinIO/COS（`--profile minio`） |
| `RETRIEVE_DRIVER` | `postgres` | 与所选向量库一致，可选 es / milvus / qdrant 等 |
| `NEO4J_ENABLE` | `true` | 要知识图谱（GraphRAG）才开，同时 compose 加 `--profile neo4j` |
| `GIN_MODE` | 生产建议 `release` | 关闭 debug 日志 |
| `JWT_SECRET` / `SYSTEM_AES_KEY` | **生产必须改成强随机值** | 默认值仅用于本地测试 |
| `OLLAMA_BASE_URL` | `http://<宿主机IP>:11434` | 容器访问宿主机 Ollama 要写 docker0 网关地址，不能写 localhost |

### 2. 模型配置（Web UI「模型管理」中配置）

首次启动后先在界面里配好模型再传文档（LLM + Embedding 为必配项）：

| 用途 | 推荐配置 | 说明 |
|------|----------|------|
| Embedding | 智谱 Embedding-3（远程 API） | 1024 维，中文效果好、价格低；预算极紧可用 Ollama 本地 `qwen3-embedding:0.6b` |
| 问答 LLM | DeepSeek（如 deepseek-v4-flash）或智谱 GLM-4.7 | 两者都有便宜的 flash/turbo 档；本地测试可接 Ollama 小模型 |
| Rerank | 智谱 rerank | 混合检索后精排，对召回质量提升明显，建议配上 |
| 多模态 VLM / ASR | 按需 | 图片描述、语音转写用，不开则跳过 |

### 3. 知识库分块（实测好用的组合）

| 配置 | 推荐值 |
|------|--------|
| 分块策略 | `auto`（自适应） |
| chunk_size / overlap | 512 / 80 |
| 分隔符 | `["\n\n", "\n", "。", "！", "？", "；", ";"]`（中文文档务必带中文标点） |
| 父子分块 | 开启，parent 4096 / child 384（用子块检索、父块喂给 LLM，检索准且上下文全） |
| 问题生成 / 自动打标签 | 按需，注意它们都烧 Token（见下文注意事项） |

## 五、注意事项（踩坑经验）

**1. 国内镜像源是第一道坎。** Docker Hub 国内直连基本失败，官方镜像 `wechatopenai/weknora-app / weknora-ui / weknora-docreader` 等都拉不下来。两个办法：一是改 Docker daemon 的 `registry-mirrors`，二是（更可靠）直接把 compose/.env 里的镜像名加上可用源前缀。笔者实际使用的镜像写法列出如下，仅供参考（源可用性随时间变化，失效请自行更换）：

| 官方镜像 | 实际使用的镜像源写法 |
|----------|----------------------|
| `wechatopenai/weknora-app:v0.7.0` | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/wechatopenai/weknora-app:v0.7.0` |
| `wechatopenai/weknora-ui:v0.7.0` | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/wechatopenai/weknora-ui:v0.7.0` |
| `wechatopenai/weknora-docreader:v0.7.0` | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/wechatopenai/weknora-docreader:v0.7.0` |
| `paradedb/paradedb:v0.22.2-pg17` | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/paradedb/paradedb:v0.22.2-pg17` |
| `library/redis:7.0-alpine` | `docker.1ms.run/library/redis:7.0-alpine` |
| `library/neo4j:2025.10.1` | `m.daocloud.io/docker.io/library/neo4j:2025.10.1` |

可选组件镜像（`minio/minio`、`qdrant/qdrant`、`milvusdb/milvus`、`semitechnologies/weaviate`、`searxng/searxng`、`langfuse/langfuse`、`apache/doris` 等）同理处理。

**2. 知识库摄入对 Token 消耗量很大，务必有预算意识。** 一篇文档入库并不只是"切片 + 向量化"：自动摘要、每块的问题生成、自动打标签、GraphRAG 图谱抽取（逐块调 LLM 抽实体关系，最贵）、Wiki 模式生成、VLM 图片描述，全都在调大模型 API。导入一个几百页的 PDF 库，Token 花费可能远超预期。建议：初期先关掉问题生成 / 图谱抽取 / Wiki，只保留最小管线；大规模导入放在额度充足的模型上；配合 `--profile langfuse` 在 Langfuse 面板里看每次调用的 Token 统计，做到心中有数。

**3. 先配模型，再传文档。** LLM 和 Embedding 模型没配好就上传文件会解析失败（这是"服务启动正常但传不了文档"的最常见原因，官方 QA 第一条就是它）。

**4. Embedding 模型要一路用到底。** 知识库创建时的 Embedding 模型决定向量维度（pgvector 索引为 1024 维），中途换 Embedding 模型意味着已有向量全部作废，需要重新解析入库。选型时一步到位。

**5. 安全与暴露面。** 官方安全声明明确建议部署在内网/私有网络，不要直接暴露公网；`JWT_SECRET`、`SYSTEM_AES_KEY`、数据库与 Redis 密码等默认值生产环境必须修改；开启登录鉴权并配好防火墙。

**6. Ollama 地址写法。** 容器里访问宿主机的 Ollama 服务，`OLLAMA_BASE_URL` 要写 `http://<宿主机IP>:11434`（Linux docker0 网关地址，用 `ip addr show docker0` 查看，默认一般是 172.17.0.1）这类地址，写 `localhost` 是容器自己，永远连不上。同时注意 WeKnora 内置 SSRF 防护，指向内网自建服务（SearXNG、内网向量库等）时需要在 `SSRF_WHITELIST` 中加白。

**7. 版本升级别偷懒。** 只执行 `docker compose up -d` 不会更新镜像，一定先 `docker compose pull`；升级前留意 CHANGELOG 是否有破坏性变更（系统会自动做数据库迁移，但仍建议先备份数据卷）。

**8. 平台兼容性。** 部分平台（如 Mac ARM）镜像内置 OCR 可能不可用，官方 QA 给出的方案是关闭 OCR 或外接 OCR 模型；生产部署优先选择 x86_64 Linux。

## 六、参考链接

- 官方仓库：https://github.com/Tencent/WeKnora
- 官方网站：https://weknora.weixin.qq.com
- 产品文档站：仓库内 `website-docs/`（约 360 个 API 端点、150 个环境变量的完整说明）
- 常见问题排查：仓库内 `docs/QA.md`
- 知识图谱配置：仓库内 `docs/KnowledgeGraph.md`
- MCP 配置：仓库内 `mcp-server/MCP_CONFIG.md`

**关键词**：WeKnora；腾讯开源；RAG；知识库；检索增强生成；ReAct Agent；Wiki 知识图谱；Docker Compose；Ubuntu 部署；大模型；Embedding；混合检索；私有化部署
