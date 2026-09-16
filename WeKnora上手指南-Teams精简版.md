# 腾讯 WeKnora 上手指南（精简版）

**是什么**：腾讯开源的企业级 RAG 知识库 + Agent 框架（github.com/Tencent/WeKnora，MIT 协议），文档解析 → 向量化 → 检索 → 大模型问答全流程私有化部署，数据自主可控。

**核心功能**
- 智能问答：RAG 检索增强问答，回答带原文引用，检索过程可视化
- Agent 推理：ReAct 多步推理，自主编排知识检索、MCP 工具与网络搜索；Agent 支持自定义配置——绑定模型、系统提示词、知识库、工具与技能、检索阈值、温度、最大迭代轮次、兜底回复等，按场景打造专属智能体
- 技能与沙箱：空间级技能目录（ClawHub / git / zip 安装），技能在会话级 Docker / E2B / Cube 沙箱中隔离执行，可跑脚本、生成文档产物
- Wiki 生成：Agent 把原始文档变成相互链接的 Markdown Wiki 与可视化知识图谱，支持人工编辑与版本回滚
- 知识管理：FAQ / 文档 / Wiki 三类库，PDF、Word、Excel、图片等十余种格式，文件夹树 + 分块在线编辑与回滚
- 数据同步：飞书 / GitLab / 腾讯 IMA / Notion / 语雀 / 钉钉 / RSS 自动同步
- 混合检索：BM25 + 向量混合召回、父子分块、GraphRAG 图谱增强、Rerank 精排
- 生态集成：20+ 模型厂商、8 种向量数据库、10 个 IM 平台、MCP Server、Chrome 插件、微信小程序
- 企业能力：RBAC 四级角色、审计日志、凭据加密、Langfuse 全链路追踪

**推荐配置**
- Embedding：首选智谱 Embedding-3，1024 维；预算紧可用 Ollama 本地模型（如 qwen3-embedding:0.6b）
- 问答模型：DeepSeek 或智谱 GLM，选便宜的 flash 档即可；本地测试可用 Ollama 跑开源小模型
- Rerank：智谱 rerank，建议开启，混合检索精排提升明显
- 分块：auto 策略，chunk_size 512，重叠 80，分隔符带中文标点
- 父子分块：建议开启，父块 4096 / 子块 384
- 基础设施：`.env` 默认值（PostgreSQL + Redis + 本地存储）即可跑通

**注意事项**
1. 国内拉镜像需加源前缀，如 `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/`、`docker.1ms.run`、`m.daocloud.io`（完整映射见完整版）
2. 知识库摄入很烧 Token：自动摘要、问题生成、图谱抽取、Wiki 生成都会调大模型 API，大库导入先关这些，配合 `--profile langfuse` 观测消耗
3. 先在界面配好 LLM + Embedding 再传文档，否则解析失败
4. Embedding 模型中途不要换，换了向量全部作废、需重新入库
5. 生产环境务必改默认密钥（JWT_SECRET / SYSTEM_AES_KEY / 数据库密码），不要暴露公网
6. 容器访问宿主机 Ollama 写 `http://<宿主机IP>:11434`（docker0 网关，`ip addr show docker0` 查看），不能写 localhost
7. 升级先 `docker compose pull` 再 `up -d`，只 `up -d` 不会更新镜像

**关键词**：WeKnora；腾讯开源；RAG；知识库；Docker；Ubuntu；私有化部署
