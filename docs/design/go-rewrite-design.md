# OpenViking Go 重写技术方案

| 项目 | 信息 |
|-----|------|
| 状态 | `草案` |
| 创建日期 | 2026-07-04 |
| 作者 | cinience |
| 关联文档 | `docs/design/parser-two-layer-refactor-plan.md`、`docs/design/session-memory-extraction-flow.md`、`docs/design/local-embedding-llama-cpp-design.md` |

---

## 目录

- [1. 概述](#1-概述)
- [2. 当前架构剖析](#2-当前架构剖析)
- [3. 设计目标与原则](#3-设计目标与原则)
- [4. 成熟 SDK 选型矩阵](#4-成熟-sdk-选型矩阵)
- [5. 目标架构](#5-目标架构)
- [6. 模块映射表](#6-模块映射表)
- [7. 详细设计](#7-详细设计)
  - [7.1 项目布局](#71-项目布局)
  - [7.2 ragfs — 文件系统范式存储](#72-ragfs--文件系统范式存储)
  - [7.3 vectordb — 向量引擎与多后端](#73-vectordb--向量引擎与多后端)
  - [7.4 ingest — 数据源接入管线](#74-ingest--数据源接入管线)
  - [7.5 parse — 文档解析两层架构](#75-parse--文档解析两层架构)
  - [7.6 retrieve — 分层检索](#76-retrieve--分层检索)
  - [7.7 session — Agent 会话与记忆](#77-session--agent-会话与记忆)
  - [7.8 models — 模型适配层](#78-models--模型适配层)
  - [7.9 server — HTTP API 与 MCP](#79-server--http-api-与-mcp)
  - [7.10 cli — 命令行与 TUI](#710-cli--命令行与-tui)
  - [7.11 bot — vikingbot 多渠道 Agent](#711-bot--vikingbot-多渠道-agent)
  - [7.12 web-studio — 前端集成](#712-web-studio--前端集成)
  - [7.13 observability — 可观测性](#713-observability--可观测性)
  - [7.14 工程基础设施](#714-工程基础设施)
  - [7.15 数据模型与 `viking://` URI](#715-数据模型与-viking-uri)
  - [7.16 ovpack 离线格式](#716-ovpack-离线格式)
- [8. 实施方案](#8-实施方案)
- [9. 测试方案](#9-测试方案)
- [10. 部署与运维](#10-部署与运维)
- [11. 兼容性与迁移策略](#11-兼容性与迁移策略)
- [12. 风险与对策](#12-风险与对策)

---

## 1. 概述

OpenViking 是面向 AI Agent 的"上下文数据库"，以**文件系统范式**统一管理 Agent 的记忆、资源与技能。当前实现是 **Python（FastAPI 主服务）+ Rust（ragfs 存储 + ov_cli）+ C++（vectordb engine Stable-ABI 扩展）+ Web Studio（Vite/React SPA）** 的混合技术栈，构建依赖 maturin + setuptools + CMake + cargo 四套工具链，部署体积大、跨语言调试困难、热路径性能受限于 Python GIL 与 PyO3 边界。

本方案设计**将完整项目用 Go 重写**，目标是：

1. **单一语言、单一二进制**：服务端、CLI、Bot 全部由 Go 实现，构建产物为一个（或少量）静态二进制，运维链路收敛。
2. **尽量采用成熟 SDK**：HTTP/RPC、向量库、文档解析、LLM 接入、MCP、可观测性等均复用社区主流库，避免自研底层。
3. **行为兼容**：保持 HTTP API、MCP 工具集、`viking://` URI 体系、L0/L1/L2 三层结构、ovpack 离线格式、配置 schema 与现状一致，让现有 Go/Python SDK 与 Web Studio 无需改动即可对接。
4. **性能持平或更优**：利用 Go 的并发模型替代 Python asyncio + Rust 桥接，热路径（embedding/upsert/hybrid search）目标达到或超过现状。

---

## 2. 当前架构剖析

### 2.1 模块清单与职责

| 模块 | 语言 | 路径 | 职责 |
|-----|-----|-----|------|
| 主服务 | Python | `openviking/` | FastAPI HTTP API + MCP 端点 + 业务编排 |
| ragfs | Rust | `crates/ragfs/` | 文件系统范式存储抽象（多 backend、多写、加密、git） |
| ragfs-cache | Rust | `crates/ragfs-cache-{redis,mooncake,yuanrong}/` | 三种缓存后端 |
| ragfs-python | Rust+PyO3 | `crates/ragfs-python/` | ragfs 的 Python 绑定，in-process 调用 |
| ov_cli | Rust | `crates/ov_cli/` | HTTP 客户端 CLI（clap + ratatui TUI） |
| vectordb engine | C++ | `src/abi3_engine_backend.cpp` | Stable-ABI 向量索引扩展（hnsw + LevelDB + SIMD 多变体） |
| Web Studio | TypeScript | `web-studio/` | React + Vite SPA |
| Go SDK | Go | `sdk/go/` | HTTP 客户端 SDK |
| Python SDK | Python | `sdk/python/openviking_sdk/` | HTTP 客户端 SDK |
| vikingbot | Python | `bot/vikingbot/` | 多渠道 Agent 框架（Telegram/飞书/DingTalk/Slack/QQ） |

### 2.2 核心流程

```
[数据源] --ingest--> [ragfs 文件系统范式存储] --embedding/upsert--> [vectordb]
                              |                                             |
                              +--queuefs (DAG: chunk/embed/upsert)----------+
                              |
[Agent query] --> [IntentAnalyzer(VLM)] --> [HierarchicalRetriever L0/L1/L2]
                              |
                              v
                       [Session 压缩 + 记忆提取] --> [memory lifecycle]
                              |
                              v
                       [MCP / REST / WebDAV]
```

### 2.3 关键约束

| 约束 | 说明 |
|-----|------|
| L0/L1/L2 三层 | 每个目录/资源附带 `.abstract`（~100 tokens）/`.overview`（~2k tokens）/原文 |
| `viking://` URI | `viking://account/...` / `viking://agent/skills` 等多租户路径 |
| 多 backend 多写 | ragfs 支持 primary + backup，带 `.sync_log.json`/`.redirect.json`/`/backend_meta.json` 元数据 |
| MCP 工具集 | `/mcp` 暴露 12 个工具（find/search/read/list/remember/add_resource/grep/glob/code_outline/code_search/code_expand/forget/health） |
| OAuth 2.1 + DCR | MCP 端点支持 OAuth 2.1 与动态客户端注册（RFC 9728） |
| ovpack 格式 | zip + manifest 的离线打包格式，用于快照与迁移 |
| 多 vectordb 后端 | volcengine / vikingdb_private / qdrant / opengauss(PG) / http / local |

---

## 3. 设计目标与原则

### 3.1 目标

| 目标 | 度量 |
|-----|-----|
| 单二进制部署 | `openviking-server`、`ov`、`vikingbot` 三个二进制，无 Python/Rust/C++ 依赖 |
| 构建时间 | 冷构建 < 60s（当前混合栈 ~6min） |
| 启动时间 | < 1s（当前 ~3s 含 Python 解释器 + 扩展加载） |
| API 兼容 | 现有 `sdk/go`、`sdk/python` 测试套件 100% 通过 |
| 性能 | 检索 P99 持平或更优；写入吞吐 ≥ 现状 1.5× |
| 内存占用 | 常驻 RSS ≤ 现状 60% |

### 3.2 原则

1. **成熟 SDK 优先**：能直接用社区库的，不自研；只有 OpenViking 独有语义（ragfs 范式、L0/L1/L2、ovpack、session 压缩）才自研。
2. **接口稳定、实现可替换**：所有外部依赖通过 Go interface 注入，便于 mock 与替换。
3. **配置 schema 兼容**：YAML/JSON 配置字段名与现状一致，迁移零成本。
4. **observability-first**：所有模块默认带 OTel span 与 Prometheus 指标。
5. **渐进迁移**：按模块分阶段实施，每个阶段都可独立运行与回滚。

---

## 4. 成熟 SDK 选型矩阵

### 4.1 基础设施

| 领域 | Python/Rust 现状 | Go 选型 | 理由 |
|-----|-----------------|---------|------|
| HTTP 框架 | FastAPI + uvicorn | **`gin-gonic/gin` v1** | Go 生态事实标准；中间件生态最广；内置 binding/validation/JSON 渲染 |
| HTTP 中间件 | fastapi middleware | `gin` 中间件 + `otelgin` + 自研 OTel/body_dump | Gin 中间件链清晰；与标准 `net/http` 集成需用 `gin.WrapH/F` 适配 |
| WebSocket | `websockets`/`python-socketio` | **`github.com/coder/websocket`**（原 nhooyr.io/websocket）| context-first、维护活跃 |
| 配置 | pydantic-settings + YAML | **`viper` + `github.com/go-playground/validator/v10`** | viper 支持 YAML/ENV/flag 多源；validator 做字段校验 |
| 日志 | loguru | **`log/slog`（标准库）+ `gopkg.in/natefinch/lumberjack.v2`** | slog 性能优秀、结构化；lumberjack 做日志轮转 |
| CLI 框架 | clap（Rust）+ typer（Python） | **`spf13/cobra` + `spf13/pflag`** | cobra 是 Go 生态事实标准 |
| TUI | ratatui（Rust） | **`charmbracelet/bubbletea` + `charmbracelet/lipgloss`** | bubbletea 是 Go TUI 主流，与 ratatui 概念对齐 |
| 数据库迁移 | 手写 SQL | `golang-migrate/migrate` | 主流迁移工具 |
| SQL 访问 | SQLAlchemy/手写 | **`sqlc`（生成代码）+ `jackc/pgx/v5`** | sqlc 类型安全；pgx 是 Postgres 最佳驱动 |
| SQLite | rusqlite + Python sqlite3 | **`modernc.org/sqlite`（纯 Go，无 cgo）** | 避免 cgo，跨平台静态编译；生产关键路径用 pgx |
| Redis | redis-py + redis crate | **`redis/go-redis/v9`** | 官方推荐 |
| S3 | aws-sdk-python + aws-sdk-rust | **`aws-sdk-go-v2`** | AWS 官方 |
| 文件监听 | pywatchman + notify | `fsnotify/fsnotify` | 跨平台主流 |
| 定时任务 | apscheduler | **`hibiken/asynq`（队列）+ `go-co-op/gocron`（定时）** | asynq 适合长任务，gocron 适合 cron |
| 异步队列 | apscheduler + 自研 queuefs | **`hibiken/asynq`** + 自研 `queuefs`（薄封装） | asynq 覆盖重试/优先级/死信；语义 DAG 仍需自研 |
| 加密 | cryptography + argon2-cffi + aes-gcm | **`crypto/aes`+`crypto/cipher`(GCM) + `alexedwards/argon2id`** | 标准库覆盖 AES-GCM；argon2id 用社区包 |
| YAML/JSON | pyyaml + pydantic | `gopkg.in/yaml.v3` + `encoding/json` + `bytedance/sonic` | sonic 性能优异，json 兼容性好 |
| 验证 | pydantic | `go-playground/validator/v10` | 主流 |

### 4.2 AI / LLM 生态

| 领域 | Python 现状 | Go 选型 | 理由 |
|-----|------------|---------|------|
| OpenAI 兼容 | `openai` SDK | **`sashabaranov/go-openai`** | 社区主流，覆盖 chat/embedding/vision |
| 多 provider 网关 | `litellm` | **`cloudwego/eino` + `eino-ext`** | 字节开源，原生支持火山引擎/通义/智谱/DeepSeek/Azure/OpenAI；OpenAI 兼容 |
| Cohere rerank | cohere SDK | `cohere-ai/cohere-toolkit` 或直接 HTTP | rerank API 简单，HTTP 客户端即可 |
| Gemini | `google-genai` | `google.golang.org/genai` 或 `google/generative-ai-go` | Google 官方 |
| Embedding 本地 | `llama-cpp-python` | `marco-o/llama-cpp-go` 或外挂 sidecar | 纯 Go 无成熟 llama.cpp 绑定，详见 [7.8](#78-models--模型适配层) |
| RAG 评估 | `ragas` | **自研 LLM-as-judge 框架** | Go 无对应库，详见 [9.5](#95-评估测试) |
| LangChain 互操作 | langchain-python | `tmc/langchaingo` | 用于 examples/integrations |

### 4.3 向量数据库

| 后端 | Python 现状 | Go 选型 |
|-----|------------|---------|
| Qdrant | qdrant-client | **`qdrant/go-client`** 官方 |
| Postgres+pgvector (opengauss) | psycopg2 | `jackc/pgx/v5` + 自写 SQL |
| 火山引擎 VikingDB | volcengine-python-sdk | **`volcengine/volc-sdk-golang`** + 强制 HTTP 直连 VikingDB |
| Local 内存 | C++ engine 扩展 | **`mnemoe/hnswlib-go` 或 `eval-vit/hnswlib-go`** + 自实现 quantization |
| HTTP 代理 | httpx | `net/http` |

### 4.4 文档与代码解析

| 领域 | Python 现状 | Go 选型 | 理由 |
|-----|------------|---------|------|
| 代码 AST | tree-sitter 全家桶 | **`smacker/go-tree-sitter`** + 各语言 binding | 已覆盖 py/js/ts/java/cpp/rust/go/c#/php/lua |
| PDF | pdfplumber + pdfminer-six | **`ledongthuc/pdf` + `pdfcpu/pdfcpu`** | pdfcpu 纯 Go 免费；ledongthuc/pdf 提取文本 |
| Word(.docx) | python-docx | **`unidoc/unioffice`** 社区版 | 纯 Go，免费版支持基础 .docx |
| Legacy .doc | olefile | 优先降级为文本提取（不支持就跳过） | .doc 是二进制格式，纯 Go 无成熟库 |
| Excel | openpyxl + xlrd | **`excelize`** | 主流，活跃维护 |
| PowerPoint | python-pptx | **`unidoc/unioffice`** | 社区版有限，大型 pptx 需自写 XML 解析 |
| EPUB | ebooklib | 自写（`archive/zip` + `goquery` 解析 xhtml） | EPUB 即 zip+xhtml，无需外部库 |
| HTML | beautifulsoup4 + trafilatura | **`golang.org/x/net/html` + `PuerkitoBio/goquery`** | 标准库 + goquery 等价 bs4 |
| 网页正文 | trafilatura + readability-lxml | `go-shiori/readability`（port） | Readability 算法的 Go 实现 |
| RSS | feedparser | **`mmcdole/gofeed`（RSS/Atom/JSON Feed）** | 主流 |
| Web 爬虫 | scrapy + playwright | **`gocolly/colly` v2 + `playwright-community/playwright-go`** | colly 是 Go 主流爬虫；playwright-go 复用 Playwright |
| 飞书 | lark-oapi | **`larksuite/oapi-sdk-go`** 官方 | 飞书官方 |
| WebDAV | 自实现 | **`studio-b12/gowebdav`** | 主流 |
| Git | gitoxide (Rust) | **`go-git/go-git` v5** | 主流，纯 Go |
| archive/zip | 标准库 | 标准库 `archive/zip` | 直接用 |

### 4.5 协议与可观测性

| 领域 | Python 现状 | Go 选型 |
|-----|------------|---------|
| MCP 协议 | `mcp` Python SDK | **`mark3labs/mcp-go`** | 主流 MCP Go 实现，覆盖 streamable HTTP 与 stdio |
| OAuth 2.1 + DCR | `mcp.server.auth.routes` | **`ory/fosite`** | 完整 OAuth 2.0/2.1 + PKCE + DCR |
| OpenTelemetry | opentelemetry-sdk + OTLP exporter | **`go.opentelemetry.io/otel`** 全家桶（trace/metric/log） + `otelhttp`/`otelgin`/`otelsql` | 官方 |
| Prometheus | prometheus_client | `prometheus/client_golang` | 官方 |
| 分布式追踪 | OTLP gRPC/HTTP | `otelexporters/otlp/...`（gRPC + HTTP） | 官方 |
| pprof | — | `net/http/pprof` | 标准库 |
| 分布式 ID | xxhash + ulid | `github.com/cespare/xxhash/v2` + `oklog/ulid` | 主流 |

### 4.6 测试与基准

| 领域 | Python 现状 | Go 选型 |
|-----|------------|---------|
| 单元测试 | pytest + pytest-asyncio | **`testing` 标准库 + `stretchr/testify`** | 标配 |
| 集成测试 | pytest + boto3 | **`testcontainers/testcontainers-go`** | 主流，覆盖 redis/qdrant/postgres/minio |
| Mock | unittest.mock | `stretchr/testify/mock` + `DATA-DOG/go-sqlmock` | 主流 |
| HTTP mock | responses | `jarcoal/httpmock` 或 `http.RoundTripper` 自实现 | 主流 |
| 基准 | pytest-benchmark + criterion（Rust） | **`testing.B` 标准库 + `benchstat`** | 标配 |
| 覆盖率 | pytest-cov | `go test -cover` + `cover` 工具 | 标配 |
| E2E | 自研 | `playwright-community/playwright-go` 驱动 Web Studio | 主流 |
| 性能回归 | 自研 benchmark/ | `benchmark/` 目录 + `benchstat` 对照 | 主流 |

---

## 5. 目标架构

```
┌────────────────────────────────────────────────────────────────────┐
│                       单一 Go 仓库 (monorepo)                       │
├────────────────────────────────────────────────────────────────────┤
│  cmd/                                                              │
│    openviking-server/   ← FastAPI 等价 HTTP 服务 + MCP + WebDAV     │
│    ov/                  ← ov_cli 等价 CLI + TUI                     │
│    vikingbot/           ← vikingbot 等价 Bot 进程                    │
│    openviking-migrate/  ← 数据库/向量库迁移工具                       │
│    openviking-doctor/   ← 配置/连通性诊断                             │
├────────────────────────────────────────────────────────────────────┤
│  internal/                                                         │
│    domain/      ← 核心领域模型（Resource/Session/Skill/...）        │
│    ragfs/       ← 文件系统范式存储（多 backend、多写、加密）          │
│    vectordb/    ← 向量引擎抽象 + adapters（qdrant/pgvector/local）  │
│    queuefs/     ← embedding/semantic DAG 队列                      │
│    ingest/      ← 数据源接入 + cursor + poller                     │
│    parse/       ← Accessor + Parser 两层                           │
│    retrieve/    ← 分层检索 + intent + memory lifecycle             │
│    session/     ← 会话 + 压缩 v2/v3 + memory + skill               │
│    models/      ← embedder/rerank/vlm 适配层                       │
│    server/      ← HTTP routers + middleware + MCP + OAuth          │
│    bot/         ← vikingbot 核心（providers/bus/channels）         │
│    observability/ ← OTel + metrics + usage audit                  │
│    privacy/     ← 隐私配置与脱敏                                    │
│    crypto/      ← AES-GCM + argon2id + envelope                   │
├────────────────────────────────────────────────────────────────────┤
│  pkg/                                                              │
│    sdk/         ← 公开 SDK（兼容现有 sdk/go，扩展）                  │
│    ovpack/     ← 离线打包格式                                      │
│    vikinguri/  ← viking:// URI 解析与规范化                         │
│    i18n/       ← 多语言（en/zh/ja）                                │
├────────────────────────────────────────────────────────────────────┤
│  web-studio/   ← Vite/React SPA（保留，由 Go embed 静态托管）        │
│  docs/         ← 文档（保留）                                       │
│  deploy/       ← Helm/Docker/K8s                                   │
└────────────────────────────────────────────────────────────────────┘
```

### 5.1 关键架构决策

| 决策 | 选择 | 备选 | 理由 |
|-----|-----|-----|------|
| 单体 vs 微服务 | **单体** | 拆分 server/worker | OpenViking 是单租户/小集群产品，单进程简化部署；用 asynq 拆出异步 worker 即可 |
| HTTP 路由 | **Gin v1** | chi/echo/fiber | Gin 中间件生态最广、内置 binding/validation；代价：与标准 `net/http` 不兼容，MCP/OAuth/WebDAV/pprof 等需用 `gin.WrapH/F` 适配（详见 7.9.2） |
| 进程模型 | **单进程 + goroutine** | 多进程 | Go GMP 调度天然并发；CPU 密集（embedding）走 asynq worker（可独立部署） |
| ragfs 实现 | **in-process Go 库** | FUSE 单独 mount | 与 Python 版一致（PyO3 in-process）；FUSE 作为可选 plugin |
| vectordb local | **hnswlib-go** | 自实现 C++ 桥接 | 纯 Go，无 cgo；生产建议走 qdrant 后端 |
| 配置 | **viper + struct** | koanf | viper 生态最广 |
| 国际化 | **i18n bundles (go-i18n)** | 手写 | 主流 |

---

## 6. 模块映射表

| Python/Rust 模块 | Go 模块 | 主要依赖 | 备注 |
|----------------|---------|---------|------|
| `openviking/server/app.py` | `internal/server/app.go` | gin + otelgin | 入口 |
| `openviking/server/routers/*` (24 个) | `internal/server/routers/*` | gin | 路径完全兼容 |
| `openviking/server/mcp_endpoint.py` | `internal/server/mcp/endpoint.go` | `mark3labs/mcp-go` | 12 工具不变 |
| `openviking/server/oauth/*` | `internal/server/oauth/*` | `ory/fosite` | DCR + OAuth 2.1 |
| `openviking/server/auth/*` | `internal/server/auth/*` | 自研 + argon2id | API key/identity |
| `openviking/service/*` | `internal/service/*` | domain + infra | 编排层 |
| `openviking/storage/viking_fs.py` | `internal/ragfs/` | 见 7.2 | in-process |
| `openviking/storage/vikingdb_manager.py` | `internal/vectordb/manager.go` | qdrant-go/pgx | 多后端 |
| `openviking/storage/vectordb_adapters/*` | `internal/vectordb/adapters/*` | 见 7.3 | 6 后端 |
| `openviking/storage/queuefs/*` | `internal/queuefs/*` | asynq + DAG | 见 7.4 |
| `openviking/storage/ovpack/*` | `pkg/ovpack/*` | archive/zip | 离线格式 |
| `openviking/ingest/*` | `internal/ingest/*` | asynq + fsnotify | 见 7.4 |
| `openviking/parse/*` | `internal/parse/*` | tree-sitter + 文档库 | 见 7.5 |
| `openviking/retrieve/*` | `internal/retrieve/*` | vectordb + models | 见 7.6 |
| `openviking/session/*` | `internal/session/*` | models + ragfs | 见 7.7 |
| `openviking/models/embedder/*` | `internal/models/embedder/*` | eino + go-openai | 见 7.8 |
| `openviking/models/rerank/*` | `internal/models/rerank/*` | HTTP client | |
| `openviking/models/vlm/*` | `internal/models/vlm/*` | eino + go-openai | |
| `openviking/core/*` | `internal/domain/*` | — | 领域模型 |
| `openviking/observability/*` | `internal/observability/*` | otel + prom | |
| `openviking/telemetry/*` | `internal/observability/tracer.go` | otel | |
| `openviking/metrics/*` | `internal/observability/metrics.go` | prom/client | |
| `openviking/privacy/*` | `internal/privacy/*` | 自研 | |
| `openviking/snapshot_namespace.py` | `internal/domain/snapshot.go` | — | |
| `crates/ragfs/` | `internal/ragfs/` | 见 7.2 | 等价重写 |
| `crates/ragfs-cache-redis/` | `internal/ragfs/cache/redis.go` | go-redis | |
| `crates/ragfs-cache-mooncake/` | （放弃或薄封装） | — | 见风险 [12.3](#123-mooncake--yuanrong-后端无法直接复用) |
| `crates/ragfs-cache-yuanrong/` | （放弃或薄封装） | — | 同上 |
| `crates/ragfs-python/` | （不需要） | — | Go 主进程直接调用 |
| `crates/ov_cli/` | `cmd/ov/` + `internal/cli/` | cobra + bubbletea | 见 7.10 |
| `src/abi3_engine_backend.cpp` | `internal/vectordb/engine/local/` | hnswlib-go | 见 7.3 |
| `bot/vikingbot/` | `cmd/vikingbot/` + `internal/bot/` | 见 7.11 | |
| `sdk/go/` | `pkg/sdk/` | 复用并扩展 | 现有代码可直接迁入 |
| `web-studio/` | 保留 | Vite/React | Go 端 embed |

---

## 7. 详细设计

### 7.1 项目布局

```
OpenViking/
├── go.mod                         # module github.com/saker-ai/ctxhub
├── go.sum
├── Makefile                       # make build/test/lint/docker
├── cmd/
│   ├── openviking-server/main.go  # HTTP 服务入口
│   ├── ov/main.go                 # CLI 入口
│   ├── vikingbot/main.go          # Bot 入口
│   ├── openviking-migrate/main.go # 迁移工具
│   └── openviking-doctor/main.go  # 诊断工具
├── internal/
│   ├── domain/                    # 领域模型（无外部依赖）
│   │   ├── resource.go
│   │   ├── session.go
│   │   ├── skill.go
│   │   ├── namespace.go           # viking:// URI
│   │   ├── identifiers.go         # peer_id/account/user
│   │   └── errors.go              # 业务错误码（与 HTTP error_mapping 对齐）
│   ├── config/                    # viper + struct + validator
│   ├── ragfs/                     # 文件系统范式存储
│   ├── vectordb/                  # 向量引擎 + adapters
│   ├── queuefs/                   # DAG 队列
│   ├── ingest/                    # 数据源接入
│   ├── parse/                     # 两层解析
│   ├── retrieve/                  # 分层检索
│   ├── session/                   # 会话与记忆
│   ├── models/                    # embedder/rerank/vlm
│   ├── service/                   # 编排层（对应 service/）
│   ├── server/                    # HTTP + MCP + OAuth
│   ├── bot/                       # vikingbot
│   ├── observability/             # OTel + Prometheus
│   ├── privacy/
│   ├── crypto/
│   └── storage/                   # 后端资源管理（连接池）
├── pkg/
│   ├── sdk/                       # 公开 SDK（迁自 sdk/go/）
│   ├── ovpack/
│   ├── vikinguri/
│   └── i18n/
├── web-studio/                    # 保留前端
├── docs/
├── deploy/
├── examples/                      # Go 示例（替换 Python examples）
└── tests/                         # E2E 与集成测试入口
```

**go.mod 关键依赖**（节选）：

```go
module github.com/saker-ai/ctxhub

go 1.24

require (
    // HTTP 与 Web
    github.com/gin-gonic/gin v1.x
    github.com/coder/websocket v1.x
    github.com/studio-b12/gowebdav v0.x

    // CLI 与 TUI
    github.com/spf13/cobra v1.x
    github.com/spf13/viper v1.x
    github.com/charmbracelet/bubbletea v1.x
    github.com/charmbracelet/lipgloss v1.x

    // 数据库与存储
    github.com/redis/go-redis/v9 v9.x
    github.com/jackc/pgx/v5 v5.x
    github.com/aws/aws-sdk-go-v2 v1.x
    modernc.org/sqlite v0.x

    // AI / LLM
    github.com/sashabaranov/go-openai v1.x
    github.com/cloudwego/eino v0.x
    github.com/cloudwego/eino-ext v0.x

    // 向量数据库
    github.com/qdrant/go-client v1.x

    // 文档解析
    github.com/smacker/go-tree-sitter v0.x
    github.com/ledongthuc/pdf v0.x
    github.com/pdfcpu/pdfcpu v0.x
    github.com/xuri/excelize/v2 v2.x
    github.com/unidoc/unioffice v0.x
    github.com/mmcdole/gofeed v1.x
    github.com/gocolly/colly/v2 v2.x
    github.com/PuerkitoBio/goquery v1.x

    // 协议与认证
    github.com/mark3labs/mcp-go v0.x
    github.com/ory/fosite v0.x

    // 可观测性
    go.opentelemetry.io/otel v1.x
    go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.x
    go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin v0.x
    github.com/prometheus/client_golang v1.x

    // 基础设施
    github.com/go-playground/validator/v10 v10.x
    github.com/hibiken/asynq v0.x
    github.com/go-git/go-git/v5 v5.x
    github.com/go-co-op/gocron/v2 v2.x
    github.com/fsnotify/fsnotify v1.x
    github.com/cespare/xxhash/v2 v2.x
    github.com/oklog/ulid/v2 v2.x
    github.com/alexedwards/argon2id v0.x
    golang.org/x/time v0.x              // rate limiting
    github.com/sony/gobreaker v1.x      // circuit breaker
    github.com/cenkalti/backoff/v4 v4.x // retry with backoff

    // 集成
    github.com/larksuite/oapi-sdk-go/v3 v3.x
    github.com/open-dingtalk/dingtalk-stream-sdk-go v0.x
    github.com/playwright-community/playwright-go v0.x

    // 测试
    github.com/stretchr/testify v1.x
    github.com/testcontainers/testcontainers-go v0.x
)
```

### 7.2 ragfs — 文件系统范式存储

#### 7.2.1 设计

ragfs 是 OpenViking 的核心抽象，提供"以路径为键、以隐藏元数据文件维护状态"的文件系统范式。Go 版本保持 in-process 库形式（与 Python 版通过 PyO3 调用 ragfs 等价），不通过 FUSE 暴露。

#### 7.2.2 核心接口

```go
// internal/ragfs/filesystem.go
package ragfs

import (
    "context"
    "io"
    "time"
)

type FileInfo struct {
    Name    string
    Size    int64
    Mode    os.FileMode
    ModTime time.Time
    IsDir   bool
}

type TreeEntry struct {
    Path     string
    RelPath  string
    Info     *FileInfo
    Extra    map[string]any
}

// FileSystem 是所有 backend 必须实现的接口。
// 对应 Rust trait: crates/ragfs/src/core/filesystem.rs::FileSystem
type FileSystem interface {
    ServicePlugin                                    // name/validate/initialize/health_check
    Create(ctx context.Context, path string, isDir bool) error
    Mkdir(ctx context.Context, path string, perm os.FileMode) error
    Remove(ctx context.Context, path string, recursive bool) error
    Read(ctx context.Context, path string, w io.Writer) error
    Write(ctx context.Context, path string, r io.Reader, perm os.FileMode) error
    ReadDir(ctx context.Context, path string) ([]*TreeEntry, error)
    Stat(ctx context.Context, path string) (*FileInfo, error)
    Rename(ctx context.Context, oldPath, newPath string) error
    Chmod(ctx context.Context, path string, perm os.FileMode) error
    Grep(ctx context.Context, pattern, path string, recursive bool) ([]GrepMatch, error)
    TreeDirectory(ctx context.Context, path string, depth int) ([]*TreeEntry, error)
}

// ServicePlugin 对应 Rust trait: core/plugin.rs::ServicePlugin
type ServicePlugin interface {
    Name() string
    Validate(config *PluginConfig) error
    Initialize(ctx context.Context, config *PluginConfig) error
    HealthCheck(ctx context.Context) error
}

// MountableFS 按路径前缀路由到已挂载 plugin（对应 core/mountable.rs）
type MountableFS struct {
    trie *radixTrie                          // github.com/armon/go-radix
    backends map[string]FileSystem
}

// MultiWriteFS primary + backup 多写（对应 core/multibackend_wrapper.rs）
type MultiWriteFS struct {
    primary   FileSystem
    backups   []FileSystem
    redirect  RedirectPolicy                  // FileOverSize / FileExtension
    syncLog   *SyncLogStore                   // .sync_log.json
}

// CacheProvider 缓存后端接口（对应 cache/provider.rs）
type CacheProvider interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Put(ctx context.Context, key string, val []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
    BatchGet(ctx context.Context, keys []string) (map[string][]byte, error)
    BatchPut(ctx context.Context, items map[string][]byte, ttl time.Duration) error
}
```

#### 7.2.3 元数据文件

| 文件 | 用途 | Go 实现 |
|-----|-----|---------|
| `.sync_log.json` | 多写一致性日志 | `internal/ragfs/synclog/store.go`（json 编码） |
| `.redirect.json` | 大文件/特定后缀重定向指针 | `internal/ragfs/redirect/redirect.go` |
| `/backend_meta.json` | 存储形状守卫（Plaintext/Encrypted） | `internal/ragfs/shape/manifest.go` |
| `.abstract` | L0 ~100 token 摘要 | `internal/ragfs/layers/abstract.go` |
| `.overview` | L1 ~2k token 概览 | `internal/ragfs/layers/overview.go` |

#### 7.2.4 Backend 清单

| Backend | Python/Rust 现状 | Go 实现 |
|--------|-----------------|---------|
| localfs | `crates/ragfs/src/plugins/localfs/` | `internal/ragfs/plugins/localfs/`（标准库 `os`） |
| s3fs | `crates/ragfs/src/plugins/s3fs/` | `internal/ragfs/plugins/s3fs/`（aws-sdk-go-v2） |
| memfs | `crates/ragfs/src/plugins/memfs/` | `internal/ragfs/plugins/memfs/`（map + RWMutex） |
| kvfs | `crates/ragfs/src/plugins/kvfs/` | `internal/ragfs/plugins/kvfs/`（基于 modernc.org/sqlite 或 goleveldb） |
| queuefs | `crates/ragfs/src/plugins/queuefs/` | `internal/ragfs/plugins/queuefs/`（封装 asynq） |
| sqlfs | `crates/ragfs/src/plugins/sqlfs/` | `internal/ragfs/plugins/sqlfs/`（pgx） |
| serverinfofs | `crates/ragfs/src/plugins/serverinfofs/` | `internal/ragfs/plugins/serverinfofs/`（自研，暴露服务健康/版本） |

#### 7.2.5 缓存层

| 缓存后端 | Go 实现 | 备注 |
|--------|---------|------|
| Memory | `internal/ragfs/cache/memory.go`（lru） | 对应 `cache/memory.rs` |
| Redis | `internal/ragfs/cache/redis.go`（go-redis） | 对应 `ragfs-cache-redis` |
| Mooncake | **不实现**（风险见 [12.3](#123-mooncake--yuanrong-后端无法直接复用)） | 可走 HTTP 代理 |
| Yuanrong | **不实现**（同上） | 可走 HTTP 代理 |

#### 7.2.6 Git 集成

| 子模块 | Rust 实现 | Go 实现 |
|------|----------|---------|
| object_store | `crates/ragfs/src/git/object_store.rs` | `internal/ragfs/git/object_store.go`（go-git） |
| ref_store | `crates/ragfs/src/git/ref_store.rs` | `internal/ragfs/git/ref_store.go`（go-git） |
| index_store | `crates/ragfs/src/git/index_store.rs` | `internal/ragfs/git/index_store.go`（go-git） |
| service | `crates/ragfs/src/git/service.rs` | `internal/ragfs/git/service.go`（go-git + 自研 commit 流程） |
| backends/local | `crates/ragfs/src/git/backends/local.rs` | `internal/ragfs/git/backends/local.go` |
| backends/s3 | `crates/ragfs/src/git/backends/s3.rs` | `internal/ragfs/git/backends/s3.go`（aws-sdk-go-v2） |

#### 7.2.7 L0/L1/L2 三层生成机制

三层隐藏文件不是用户手写的，而是 ingest/parse 管线在资源写入后由 queuefs DAG 自动生成。

| 层级 | 文件 | 大小 | 生成时机 | 用途 |
|-----|------|-----|---------|------|
| L0 Abstract | `.abstract` | ~100 tokens | 资源 upsert 完成后同步生成 | 目录定位/粗筛 |
| L1 Overview | `.overview` | ~2k tokens | DAG 节点 `understanding_parse_processor` 异步生成 | 候选目录二次检索 |
| L2 Details | 原文 + chunks | 全量 | parse 阶段切片 + embedding | 细节检索 |

**生成流程**：

```
[parse 完成] → [chunks 写入 vectordb]
                    │
                    ▼
              [L0 生成]（同步，~100 tokens 摘要）
                    │      ├─ prompt: prompts/templates/abstract.yaml
                    │      └─ VLM: internal/models/vlm
                    ▼
              [L1 生成]（异步 DAG 节点）
                    │      ├─ prompt: prompts/templates/overview.yaml
                    │      └─ 输入: L0 + chunks 摘要
                    ▼
              [更新 ragfs 元数据]（.abstract / .overview）
```

**失效与更新策略**：

| 触发条件 | 行为 |
|---------|-----|
| 资源内容变更（写入新版本） | 重新生成 L0/L1，旧版本保留为 `.abstract.{version}.bak` |
| 资源删除 | 删除隐藏文件 + 向量库级联删除 |
| VLM 模型版本升级 | 通过 `openviking-migrate reabstract` 批量重生成 |
| L1 生成失败 | 重试 3 次后降级为 L0 复用，标记 `.overview.fallback=true` |

**Go 实现**：

```go
// internal/ragfs/layers/generator.go
type LayerGenerator interface {
    GenerateAbstract(ctx context.Context, res *Resource) (string, error)
    GenerateOverview(ctx context.Context, res *Resource) (string, error)
}

type VLMGenerator struct {
    vlm       vlm.VLM
    templates *template.Store       // 加载 prompts/templates/*.yaml
}

// internal/queuefs/processors/understanding.go
func (p *UnderstandingProcessor) Process(ctx context.Context, msg *UnderstandingMsg) error {
    res, _ := p.fs.ReadResource(ctx, msg.ResourceURI)
    if abs, _ := p.fs.ReadHidden(ctx, msg.ResourceURI, ".abstract"); abs == "" {
        generated, _ := p.gen.GenerateAbstract(ctx, res)
        p.fs.WriteHidden(ctx, msg.ResourceURI, ".abstract", generated)
    }
    overview, _ := p.gen.GenerateOverview(ctx, res)
    return p.fs.WriteHidden(ctx, msg.ResourceURI, ".overview", overview)
}
```

#### 7.2.8 并发与一致性模型

ragfs 多 backend 多写场景下，需要明确一致性语义。Go 版**保持与 Rust 版一致**：**primary 强一致 + backup 最终一致**。

| 机制 | 实现 | 元数据文件 |
|-----|-----|----------|
| 路径锁 | `internal/ragfs/lock/path_lock.go`（基于 Redis SETNX 或本地 mutex） | — |
| 事务日志 | `internal/ragfs/transaction/redo_log.go`（WAL + checksum） | `.redo_log.json` |
| 多写同步 | `internal/ragfs/synclog/store.go`（OpType + ts + backend 状态） | `.sync_log.json` |
| 重定向 | `internal/ragfs/redirect/redirect.go`（FileOverSize/FileExtension） | `.redirect.json` |
| 形状守卫 | `internal/ragfs/shape/manifest.go`（Plaintext/Encrypted） | `/backend_meta.json` |

**一致性算法**：

```
write(path, data):
  1. acquire path_lock(path)
  2. append redo_log(op=WRITE, path, data_hash)
  3. write to primary backend (sync, return on success)
  4. fanout to backup backends (async, with retry)
       └─ on success: append sync_log(op=SYNC, backend, ts)
       └─ on failure: append sync_log(op=PENDING, backend, retry_count)
  5. release path_lock

read(path):
  1. check redirect.json (if redirected, follow pointer)
  2. read from primary backend
  3. if primary fails: read from backup, log inconsistency

reconcile():
  - 周期任务扫描 .sync_log.json 的 PENDING 记录
  - 重试未同步的 op
  - 超过 max_retries 转入死信队列
```

**事务边界**：

| 操作类型 | 事务范围 |
|---------|---------|
| 单路径写 | path_lock + redo_log |
| 跨路径原子写（如 rename） | 多 path_lock + 单 redo_log（多 op） |
| 资源 + 向量 upsert | 跨 ragfs 与 vectordb，**软事务**：ragfs 提交后异步触发 vectordb upsert，失败由 queuefs 重试 |
| ovpack 导入 | 整包原子（zip 内 manifest 校验通过后一次性提交） |

**Go 实现要点**：

- 路径锁用 `sync.Map` + `*sync.Mutex` per-path（本地）；分布式部署用 Redis SETNX + TTL
- redo_log 用 `bbolt`（B+ tree，ACID）或追加文件 + fsync
- sync_log 用 JSON 编码（与 Rust 版兼容，可双向读取）
- 所有锁获取遵循**确定性顺序**（按 path 字典序）避免死锁

### 7.3 vectordb — 向量引擎与多后端

#### 7.3.1 架构

```
internal/vectordb/
├── engine.go                # IndexEngine 接口
├── manager.go               # VikingDBManager 等价（Collection 生命周期）
├── schema.go                # Collection schemas
├── adapters/                # CollectionAdapter 实现
│   ├── factory.go           # _ADAPTER_REGISTRY 等价
│   ├── base.go              # CollectionAdapter 接口
│   ├── local.go             # local 后端（hnswlib-go）
│   ├── http.go              # HTTP 代理
│   ├── qdrant.go            # qdrant-go
│   ├── opengauss.go         # pgx + pgvector
│   ├── volcengine.go        # volc-sdk-golang + HTTP
│   └── vikingdb_private.go  # 内部 SDK
├── engine/
│   └── local/               # 对应 src/abi3_engine_backend.cpp
│       ├── hnsw.go          # hnswlib-go 包装
│       ├── kv_store.go      # modernc.org/sqlite 作 KV
│       ├── schema.go        # Schema/BytesRow
│       └── simd.go          # 标量 fallback + build tags（amd64 优先 SSE/AVX）
└── migration.go             # vector_migration 等价
```

#### 7.3.2 IndexEngine 接口

```go
// internal/vectordb/engine.go
type IndexEngine interface {
    Add(ctx context.Context, collection string, rows []VectorRow) error
    Delete(ctx context.Context, collection string, ids []string) error
    Search(ctx context.Context, req SearchRequest) (*SearchResult, error)
    Dump(ctx context.Context, collection string, w io.Writer) error
    GetState(ctx context.Context, collection string) (*CollectionState, error)
}

type CollectionAdapter interface {
    CreateCollection(ctx context.Context, schema CollectionSchema) error
    DropCollection(ctx context.Context, name string) error
    ListCollections(ctx context.Context) ([]string, error)
    Upsert(ctx context.Context, collection string, rows []VectorRow) error
    Search(ctx context.Context, req SearchRequest) (*SearchResult, error)
    HybridSearch(ctx context.Context, req HybridSearchRequest) (*SearchResult, error)  // dense + sparse
    Delete(ctx context.Context, collection string, ids []string) error
    GetState(ctx context.Context, collection string) (*CollectionState, error)
}
```

#### 7.3.3 Local 后端

| 组件 | Python/C++ 现状 | Go 实现 |
|------|----------------|---------|
| HNSW 索引 | C++ `vectordb::IndexEngine` | **`github.com/eval-vit/hnswlib-go`** 或 `mnemoe/hnswlib-go` |
| KV store | C++ `KVStore`（LevelDB） | `modernc.org/sqlite` 或 `go.etcd.io/bbolt` |
| SIMD | x86 SSE3/AVX2/AVX512 多变体 | Go 标量实现 + `build` tags 切换；如需 SIMD 走 cgo + `github.com/klauspost/cpuid/v2` 探测 |
| 距离度量 | cosine/L2/IP | hnswlib-go 原生支持 |

> **决策**：local 后端默认走 hnswlib-go（无 cgo）。生产部署强烈建议走 qdrant 后端（[12.2](#122-local-vectordb-性能)）。

### 7.4 ingest — 数据源接入管线

#### 7.4.1 模块映射

| Python 文件 | Go 文件 | 职责 |
|-----------|---------|------|
| `ingest/registry.py` | `internal/ingest/registry.go` | `@register_source` 等价 |
| `ingest/orchestrator.py` | `internal/ingest/orchestrator.go` | backfill 编排 |
| `ingest/poller.py` | `internal/ingest/poller.go` | watch + asynq scheduler |
| `ingest/cursor_store.py` | `internal/ingest/cursor_store.go` | SQLite 游标（modernc.org/sqlite） |
| `ingest/replay.py` | `internal/ingest/replay.go` | 归一化 replay 到 session |
| `ingest/peer.py` | `internal/ingest/peer.go` | peer_id 协调 |
| `ingest/normalize.py` | `internal/ingest/normalize.go` | 消息归一化 |
| `ingest/sources/base.py` | `internal/ingest/sources/base.go` | `JsonlLogSource`/`SqliteLogSource` 抽象 |
| `ingest/sources/claude_code.py` | `internal/ingest/sources/claude_code.go` | JSONL 解析 |
| `ingest/sources/codex.py` | `internal/ingest/sources/codex.go` | |
| `ingest/sources/cursor.py` | `internal/ingest/sources/cursor.go` | SQLite 解析 |
| `ingest/sources/hermes.py` | `internal/ingest/sources/hermes.go` | |
| `ingest/sources/openclaw.py` | `internal/ingest/sources/openclaw.go` | |
| `ingest/sources/opencode.py` | `internal/ingest/sources/opencode.go` | |

#### 7.4.2 Poller 替换

Python 版用 `apscheduler` + 自研 watch。Go 版改为：

- **`fsnotify/fsnotify`**：本地文件变更监听
- **`hibiken/asynq`**：异步任务队列（重试/优先级/死信）
- **`go-co-op/gocron`**：cron 式调度（替代 apscheduler 的 cron 触发器）

#### 7.4.3 queuefs DAG 调度

Python `storage/queuefs/semantic_dag.py` 实现了一个语义 DAG 调度器，节点间有依赖关系。Go 版保持等价语义，但底层用 asynq 做实际分发。

**DAG 节点类型**：

| 节点 | 输入 | 输出 | 依赖 |
|-----|-----|------|-----|
| `chunk` | 资源内容 | chunks[] | — |
| `embed` | chunks[] | vectors[] | `chunk` |
| `upsert` | vectors[] | vectordb 状态 | `embed` |
| `understanding_parse` | 资源内容 | L0 abstract | `chunk` |
| `understanding_overview` | chunks 摘要 | L1 overview | `understanding_parse` |
| `index_abstract` | L0 abstract | abstract 向量 | `understanding_parse` |
| `index_overview` | L1 overview | overview 向量 | `understanding_overview` |

**asynq mapping**：

| DAG 节点 | asynq task type | 优先级 | 重试 |
|---------|----------------|-------|-----|
| `chunk` | `queuefs:chunk` | high | 3 |
| `embed` | `queuefs:embed` | default | 5 |
| `upsert` | `queuefs:upsert` | default | 5 |
| `understanding_parse` | `queuefs:understanding_parse` | low | 3 |
| `understanding_overview` | `queuefs:understanding_overview` | low | 3 |
| `index_abstract` | `queuefs:index_abstract` | default | 5 |
| `index_overview` | `queuefs:index_overview` | default | 5 |

**调度算法**：

```go
// internal/queuefs/scheduler.go
type Scheduler struct {
    client *asynq.Client
    inspector *asynq.Inspector
    dag    *DAGSpec
}

func (s *Scheduler) Submit(ctx context.Context, root *RootTask) error {
    // 1. 入库 DAG 实例（SQLite，持久化以支持重启恢复）
    instID := s.dagStore.Create(ctx, root)

    // 2. 拓扑排序后入队所有入度为 0 的节点
    ready := s.dag.Roots()
    for _, node := range ready {
        payload := encodePayload(instID, node.ID, node.Input)
        _, err := s.client.EnqueueContext(ctx,
            asynq.NewTask(node.Type, payload),
            asynq.TaskID(node.TaskID()),
            asynq.Priority(node.Priority),
            asynq.Retries(node.Retries),
        )
        if err != nil { return err }
    }
    return nil
}

// Handler 完成后调用 markDone，触发下游节点
func (h *Handler) Handle(ctx context.Context, t *asynq.Task) error {
    node, _ := decodePayload(t.Payload())
    result, err := h.execute(ctx, node)
    if err != nil { return err }   // asynq 自动重试

    s.dagStore.MarkDone(ctx, node.InstID, node.ID, result)
    next := s.dag.Dependents(node.ID)
    for _, n := range next {
        if s.dagStore.AllDepsDone(ctx, node.InstID, n.ID) {
            s.client.EnqueueContext(ctx, asynq.NewTask(n.Type, ...))
        }
    }
    return nil
}
```

**失败与恢复**：

| 场景 | 处理 |
|-----|------|
| 节点重试耗尽 | asynq 进入死信队列；DAG 实例标记 `partial_failed`；后台 reconciler 周期重试 |
| Worker 崩溃 | asynq 自动重投递（visibility timeout） |
| SQLite 损坏 | DAG 实例丢失，但 asynq 队列仍存活；从 asynq 恢复 |
| 资源被删除 | 检测到 `resource_deleted` 事件，取消相关 DAG 实例（asynq `DeleteTask`） |

**Go 实现要点**：

- DAG spec 用声明式 YAML（`queuefs/dags/*.yaml`），编译期校验无环
- asynq 优先级：高优先级给付费/交互路径，低优先级给 understanding
- embedding 批量大小与并发度按模型 provider 限速（如 OpenAI 5000 RPM），用 `golang.org/x/time/rate` 控速
- DAG 实例状态用 SQLite 持久化（`dag_instances` / `dag_nodes` 两张表）

### 7.5 parse — 文档解析两层架构

参考 [`docs/design/parser-two-layer-refactor-plan.md`](parser-two-layer-refactor-plan.md)，保持 **Accessor（L1 数据访问）+ Parser（L2 数据解析）** 两层。

#### 7.5.1 Accessor 清单

| Python 文件 | Go 文件 | 依赖 |
|-----------|---------|------|
| `accessors/local.py` | `internal/parse/accessors/local.go` | 标准库 |
| `accessors/http.py` | `internal/parse/accessors/http.go` | net/http |
| `accessors/git.py` | `internal/parse/accessors/git.go` | go-git |
| `accessors/webfeed.py` | `internal/parse/accessors/webfeed.go` | gofeed |
| `accessors/feishu.py` | `internal/parse/accessors/feishu.go` | larksuite/oapi-sdk-go |
| `accessors/web_crawler/` | `internal/parse/accessors/webcrawler/` | colly + playwright-go |

#### 7.5.2 Parser 清单

| 后缀 | Python Parser | Go Parser | 依赖 |
|-----|--------------|-----------|------|
| `.md` | `parsers/markdown.py` | `internal/parse/parsers/markdown.go` | `yuin/goldmark` |
| `.txt` | `parsers/text.py` | `internal/parse/parsers/text.go` | 标准库 |
| `.pdf` | `parsers/pdf.py` | `internal/parse/parsers/pdf.go` | `ledongthuc/pdf` + `pdfcpu/pdfcpu` |
| `.html` | `parsers/html.py` | `internal/parse/parsers/html.go` | `goquery` + `readability` |
| `.docx` | `parsers/word.py` | `internal/parse/parsers/word.go` | `unioffice` |
| `.doc` | `parsers/legacy_doc.py` | `internal/parse/parsers/legacy_doc.go` | 降级为文本提取（olefile 无 Go 等价） |
| `.pptx` | `parsers/powerpoint.py` | `internal/parse/parsers/powerpoint.go` | `unioffice` |
| `.xlsx` | `parsers/excel.py` | `internal/parse/parsers/excel.go` | `xuri/excelize/v2` |
| `.epub` | `parsers/epub.py` | `internal/parse/parsers/epub.go` | `archive/zip` + `goquery`（自写） |
| `.zip` | `parsers/zip_parser.py` | `internal/parse/parsers/zip.go` | 标准库 `archive/zip` |
| dir | `parsers/directory.py` | `internal/parse/parsers/directory.go` | 标准库 |
| image/audio/video | `parsers/media/` | `internal/parse/parsers/media/` | 调用 VLM（详见 7.8） |
| code | `parsers/code/` | `internal/parse/parsers/code/` | `smacker/go-tree-sitter` |
| feishu doc | `parsers/feishu.py` | `internal/parse/parsers/feishu.go` | larksuite/oapi-sdk-go |

#### 7.5.3 tree-sitter 语言 binding

| 语言 | Rust binding | Go binding |
|-----|-------------|-----------|
| Python | tree-sitter-python | `smacker/go-tree-sitter/python` |
| JavaScript | tree-sitter-javascript | `smacker/go-tree-sitter/javascript` |
| TypeScript | tree-sitter-typescript | `smacker/go-tree-sitter/typescript` |
| Java | tree-sitter-java | `smacker/go-tree-sitter/java` |
| C++ | tree-sitter-cpp | `smacker/go-tree-sitter/cpp` |
| Rust | tree-sitter-rust | `smacker/go-tree-sitter/rust` |
| Go | tree-sitter-go | `smacker/go-tree-sitter/golang` |
| C# | tree-sitter-c-sharp | `smacker/go-tree-sitter/csharp` |
| PHP | tree-sitter-php | `smacker/go-tree-sitter/php` |
| Lua | tree-sitter-lua | `smacker/go-tree-sitter/lua` |

### 7.6 retrieve — 分层检索

#### 7.6.1 模块映射

| Python 文件 | Go 文件 | 职责 |
|-----------|---------|------|
| `retrieve/hierarchical_retriever.py` | `internal/retrieve/hierarchical.go` | L0/L1/L2 分层检索 |
| `retrieve/intent_analyzer.py` | `internal/retrieve/intent.go` | VLM query planning |
| `retrieve/memory_lifecycle.py` | `internal/retrieve/memory_lifecycle.go` | hotness score |
| `retrieve/retrieval_stats.py` | `internal/retrieve/stats.go` | 指标收集 |

#### 7.6.2 HierarchicalRetriever 流程

```
query
  │
  ▼
IntentAnalyzer(VLM) ──> rewrite + subqueries + level hint
  │
  ▼
[Level 0: 抽象层 dense + sparse hybrid]
  │   └─ vectordb.HybridSearch(collection=*.abstract, ...)
  ▼
[Level 1: 概览层]
  │   └─ 在候选目录的 *.overview 中二次检索
  ▼
[Level 2: 细节层]
  │   └─ 在候选资源的原文 chunk 中检索
  ▼
Rerank（Cohere/OpenAI/Volcengine）
  │
  ▼
Memory lifecycle（hotness 加权）
  │
  ▼
返回 + Stats span
```

#### 7.6.3 接口

```go
type Retriever interface {
    Retrieve(ctx context.Context, req RetrieveRequest) (*RetrieveResponse, error)
}

type HybridSearcher interface {
    HybridSearch(ctx context.Context, req HybridSearchRequest) (*SearchResult, error)
}

type Reranker interface {
    Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error)
}

type IntentAnalyzer interface {
    Analyze(ctx context.Context, query string) (*Intent, error)
}
```

### 7.7 session — Agent 会话与记忆

#### 7.7.1 模块映射

| Python 文件 | Go 文件 | 职责 |
|-----------|---------|------|
| `session/session.py` | `internal/session/session.go` | Session 对象 |
| `session/compressor_v2.py` | `internal/session/compressor_v2.go` | 对话压缩 v2 |
| `session/compressor_v3.py` | `internal/session/compressor_v3.go` | 对话压缩 v3 |
| `session/memory/` | `internal/session/memory/` | 记忆管理 |
| `session/memory_policy.py` | `internal/session/memory_policy.go` | 记忆策略 |
| `session/skill/` | `internal/session/skill/` | 技能 |
| `session/train/` | `internal/session/train/` | 训练 |
| `session/tool_result_store.py` | `internal/session/tool_result_store.go` | 工具结果外化 |
| `session/tool_result_synopsis.py` | `internal/session/tool_result_synopsis.go` | 工具结果摘要 |
| `session/tool_skill_utils.py` | `internal/session/tool_skill_utils.go` | 工具-技能桥接 |

#### 7.7.2 压缩算法

compressor v2/v3 算法与 prompt template 需逐行迁移。Go 版：

- prompt 模板用 `text/template` 或 `github.com/Masterminds/sprig/v3` 加载 YAML 配置（对应 `prompts/templates/**/*.yaml`）
- LLM 调用走 `internal/models/vlm`
- 输出 JSON 解析用 `bytedance/sonic` + `json-repair` 等价（`github.com/RealAlexChen/json-repair` 或自写）

#### 7.7.3 触发条件、v2/v3 差异与记忆提取输出

**触发条件**（与 Python 版一致）：

| 触发器 | 阈值 | 动作 |
|-------|-----|------|
| 上下文 token 计数 | 当前 session 对话 token > `max_context_tokens`（默认 32000） | 启动压缩 |
| 轮次计数 | session 轮次 > `max_turns`（默认 20） | 启动压缩 |
| 显式调用 | API `POST /api/v1/sessions/{id}/commit` | 强制压缩并归档 |
| 工具结果体积 | 单条工具结果 > `tool_result_synopsis_threshold`（默认 4KB） | 工具结果外化 + synopsis 替换 |

**v2 vs v3 差异**：

| 维度 | compressor_v2 | compressor_v3 |
|-----|--------------|--------------|
| 算法 | 全量对话送 VLM，输出压缩后的对话 + 摘要 | 分段压缩：先按 turn 切片，每片独立摘要，再合并 |
| 适用场景 | 中短对话（< 50 turns） | 长对话（≥ 50 turns），或工具结果密集场景 |
| 记忆提取 | 压缩时同步提取，输出扁平 memory 列表 | 分两阶段：压缩 → 异步深度提取（`memory/train/`） |
| prompt 模板 | `prompts/templates/compressor_v2.yaml` | `prompts/templates/compressor_v3/*.yaml`（多模板组合） |
| 失败回退 | 报错给调用方 | 自动降级为 v2 |
| token 成本 | 较高（单次大 prompt） | 较低（多次小 prompt，可并发） |

**默认策略**：

```go
// internal/session/compressor_factory.go
func NewCompressor(cfg CompressorConfig) Compressor {
    switch cfg.Version {
    case "v2":  return &CompressorV2{vlm: cfg.VLM, tpl: cfg.Templates}
    case "v3":  return &CompressorV3{vlm: cfg.VLM, tpl: cfg.Templates, train: cfg.Train}
    case "auto":
        // 按对话长度自动选择
        return &AutoCompressor{
            v2: NewCompressor(withV2(cfg)),
            v3: NewCompressor(withV3(cfg)),
            threshold: cfg.AutoV3Turns,   // 默认 50
        }
    }
}
```

**记忆提取输出结构**：

```go
// internal/session/memory/types.go
type ExtractedMemory struct {
    ID          string            `json:"id"`           // ulid
    Type        MemoryType        `json:"type"`         // fact / preference / skill / event / relation
    Content     string            `json:"content"`      // 自然语言描述
    SourceTurns []int             `json:"source_turns"` // 提取自哪些 turn
    Confidence  float64           `json:"confidence"`   // 0-1
    Metadata    map[string]any    `json:"metadata,omitempty"`
    CreatedAt   time.Time         `json:"created_at"`
}

type MemoryDiff struct {
    Added    []ExtractedMemory `json:"added"`
    Updated  []ExtractedMemory `json:"updated"`   // 与已有 memory 合并后的新版本
    Archived []string          `json:"archived"`  // 被替换/失效的 memory ID
}
```

**合并策略**（`memory/merge_op/`）：

| 操作 | 触发 | 实现 |
|-----|------|-----|
| `add` | 新事实，与已有 memory 无冲突 | 直接插入 ragfs `/memory/{type}/{id}.json` + 向量化 |
| `update` | 已有 memory 内容补充 | 读旧 → LLM 合并 → 写新版本，旧版本 `.bak` |
| `archive` | 新事实使旧 memory 失效（如"用户已换工作"） | 旧 memory 移到 `/memory/.archive/`，新 memory 入位 |
| `split` | 单条 memory 过长或含多事实 | LLM 拆分为多条，旧条 archive |

**Go 实现要点**：

- prompt 模板加载到 `*template.Store`，热更新通过 fsnotify 监听 `prompts/templates/`
- VLM 调用走 `internal/models/vlm`，带熔断（`sony/gobreaker`）+ 重试（`cenkalti/backoff`）
- 记忆提取的 JSON 输出可能损坏，用 `json-repair` 等价库修复后再解析
- 所有 memory 写入触发 `index_abstract` DAG 节点（参考 [7.4.3](#743-queuefs-dag-调度)）

### 7.8 models — 模型适配层

#### 7.8.1 Embedder 接口

```go
// internal/models/embedder/embedder.go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dim() int
    Model() string
}
```

#### 7.8.2 Provider 清单

| Provider | Python 实现 | Go 实现 | 依赖 |
|---------|-----------|---------|------|
| OpenAI | `openai_embedders.py` | `internal/models/embedder/openai.go` | `sashabaranov/go-openai` |
| Volcengine | `volcengine_embedders.py` | `internal/models/embedder/volcengine.go` | `cloudwego/eino-ext` + HTTP |
| VikingDB | `vikingdb_embedders.py` | `internal/models/embedder/vikingdb.go` | volc-sdk-golang |
| DashScope | `dashscope_embedders.py` | `internal/models/embedder/dashscope.go` | HTTP（无官方 Go SDK） |
| Cohere | `cohere_embedders.py` | `internal/models/embedder/cohere.go` | HTTP |
| Jina | `jina_embedders.py` | `internal/models/embedder/jina.go` | HTTP |
| Minimax | `minimax_embedders.py` | `internal/models/embedder/minimax.go` | HTTP |
| Voyage | `voyage_embedders.py` | `internal/models/embedder/voyage.go` | HTTP |
| Gemini | `gemini_embedders.py` | `internal/models/embedder/gemini.go` | `google.golang.org/genai` |
| LiteLLM | `litellm_embedders.py` | `internal/models/embedder/litellm.go` | HTTP（统一网关） |
| Local | `local_embedders.py` | `internal/models/embedder/local.go` | 走 sidecar HTTP（详见 7.8.5） |

#### 7.8.3 VLM Provider

| Provider | Go 实现 | 备注 |
|---------|---------|------|
| OpenAI | `internal/models/vlm/openai.go` | `sashabaranov/go-openai` |
| Volcengine | `internal/models/vlm/volcengine.go` | `eino-ext` 火山引擎 |
| LiteLLM | `internal/models/vlm/litellm.go` | HTTP 统一网关 |
| Codex（OAuth） | `internal/models/vlm/codex.go` | 移植 `codex_auth.py` + `codex_responses_adapter.py` |
| GLM | `internal/models/vlm/glm.go` | HTTP |
| Kimi | `internal/models/vlm/kimi.go` | HTTP |

#### 7.8.4 Rerank Provider

| Provider | Go 实现 | 备注 |
|---------|---------|------|
| Cohere | `internal/models/rerank/cohere.go` | HTTP |
| OpenAI | `internal/models/rerank/openai.go` | HTTP（go-openai 暂无 rerank，自写） |
| Volcengine | `internal/models/rerank/volcengine.go` | HTTP |
| LiteLLM | `internal/models/rerank/litellm.go` | HTTP |

#### 7.8.5 本地 embedding（llama-cpp 等价）

Python 版用 `llama-cpp-python` in-process。Go 端选项：

| 方案 | 优点 | 缺点 |
|-----|------|------|
| **A. HTTP sidecar**（推荐） | 解耦；可用 `Ollama`/`llama.cpp server` | 需独立进程 |
| B. cgo 绑定 `llama.cpp` | 无独立进程 | cgo 编译复杂、跨平台难 |
| C. 纯 Go 实现（如 `tao-go/llama`） | 无外部依赖 | 性能与模型生态不成熟 |

**决策**：默认走 **方案 A**，通过 `LOCAL_EMBED_BASE_URL` 指向 Ollama/llama.cpp 服务。

### 7.9 server — HTTP API 与 MCP

#### 7.9.1 路由清单（与 Python 100% 对齐）

| Prefix | 模块 | 备注 |
|-------|------|------|
| `/api/v1/admin` | `internal/server/routers/admin.go` | 账户/用户/密钥管理 |
| `/api/v1/resources` | `routers/resources.go` | 资源 CRUD |
| `/api/v1/fs` | `routers/filesystem.go` | 文件系统范式 |
| `/api/v1/content` | `routers/content.go` | 内容读写 |
| `/api/v1/console` | `routers/console.go` | 控制台查询 |
| `/api/v1/search` | `routers/search.go` | 检索 |
| `/api/v1/code` | `routers/code.go` | 代码 outline/search/expand |
| `/api/v1/relations` | `routers/relations.go` | 关系图 |
| `/api/v1/sessions` | `routers/sessions.go` | Session |
| `/api/v1/skills` | `routers/skills.go` | 技能 |
| `/api/v1/snapshot` | `routers/snapshot.go` | 快照 |
| `/api/v1/stats` | `routers/stats.go` | 统计 |
| `/api/v1/pack` | `routers/pack.go` | ovpack 导出 |
| `/api/v1/privacy-configs` | `routers/privacy_configs.go` | 隐私配置 |
| `/api/v1/debug` | `routers/debug.go` | 调试 |
| `/api/v1/observer` | `routers/observer.go` | 观察者 |
| `/api/v1/tasks` | `routers/tasks.go` | 任务跟踪 |
| `/api/v1/user-settings` | `routers/user_settings.go` | 用户设置 |
| `/api/v1/watches` | `routers/watches.go` | watch |
| `/api/v1/oauth/*` | `routers/oauth.go` | OAuth 子路由 |
| `/api/v1/system` | `routers/system.go` | 系统 |
| `/webdav/resources` | `routers/webdav.go` | WebDAV |
| `/bot/v1` | `routers/bot.go` | Bot 网关代理 |
| `/mcp` | `internal/server/mcp/endpoint.go` | MCP streamable HTTP |
| `/metrics` | `internal/observability/metrics.go` | Prometheus |
| `/studio/*` | `internal/server/static.go` | Web Studio（embed） |

#### 7.9.2 中间件链

```go
// internal/server/middleware.go
import "github.com/gin-gonic/gin"
import "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

r := gin.New()
r.Use(requestIDMiddleware)           // 生成 X-Request-ID
r.Use(gin.Recovery())                // panic 恢复
r.Use(otelgin.Middleware("openviking"))        // OTel root span
r.Use(corsMiddleware)                // CORS
r.Use(bodyDumpMiddleware)            // 对应 body_dump_middleware.py
r.Use(httpObservabilityMiddleware)   // RED metrics
r.Use(profileMiddleware)             // 性能采样
r.Use(headerLoggingMiddleware)       // header log
r.Use(timingMiddleware)              // X-Process-Time

// === net/http 适配层 ===
// MCP / OAuth / WebDAV / pprof / metrics / static 等使用标准 http.Handler
// 的第三方库，统一通过 gin.WrapH / gin.WrapF 挂载到 Gin。
//
// 例 1：MCP streamable HTTP（mark3labs/mcp-go 自带 net/http server）
r.Any("/mcp", gin.WrapH(mcpServer))
r.Any("/mcp/*any", gin.WrapH(mcpServer))   // SSE 子路径
//
// 例 2：pprof
r.GET("/debug/pprof/*any", gin.WrapF(pprof.Index))
r.GET("/debug/pprof/cmdline", gin.WrapF(pprof.Cmdline))
r.GET("/debug/pprof/profile", gin.WrapF(pprof.Profile))
r.GET("/debug/pprof/symbol", gin.WrapF(pprof.Symbol))
r.GET("/debug/pprof/trace", gin.WrapF(pprof.Trace))
//
// 例 3：Prometheus metrics
r.GET("/metrics", gin.WrapF(promhttp.Handler()))
//
// 例 4：WebDAV（gowebdav 或自实现 http.Handler）
r.Any("/webdav/resources/*any", gin.WrapH(webdavHandler))
//
// 例 5：Web Studio 静态资源（embed.FS）
r.StaticFS("/studio", http.FS(studioFS))
//
// 例 6：OAuth 2.1（fosite 是标准 net/http）
r.Any("/oauth/*any", gin.WrapH(oauthHandler))
r.GET("/.well-known/oauth-authorization-server", gin.WrapF(wellknownAuthServer))
r.GET("/.well-known/oauth-protected-resource", gin.WrapF(wellknownProtectedResource))
```

#### 7.9.3 MCP 端点

```go
// internal/server/mcp/endpoint.go
import "github.com/mark3labs/mcp-go/mcp"
import "github.com/mark3labs/mcp-go/server"

srv := server.NewMCPServer("openviking", version)
srv.AddTool(findTool,      mcp.NewTool("find", ...))
srv.AddTool(searchTool,    mcp.NewTool("search", ...))
srv.AddTool(readTool,      mcp.NewTool("read", ...))
srv.AddTool(listTool,      mcp.NewTool("list", ...))
srv.AddTool(rememberTool,  mcp.NewTool("remember", ...))
srv.AddTool(addResourceTool, mcp.NewTool("add_resource", ...))
srv.AddTool(grepTool,      mcp.NewTool("grep", ...))
srv.AddTool(globTool,      mcp.NewTool("glob", ...))
srv.AddTool(codeOutlineTool, mcp.NewTool("code_outline", ...))
srv.AddTool(codeSearchTool, mcp.NewTool("code_search", ...))
srv.AddTool(codeExpandTool, mcp.NewTool("code_expand", ...))
srv.AddTool(forgetTool,    mcp.NewTool("forget", ...))
srv.AddTool(healthTool,    mcp.NewTool("health", ...))

// streamable HTTP 挂在 /mcp
srv.SetBasicAuth 或 OAuth2.1（fosite）
http.ListenAndServe(":8080", srv)
```

#### 7.9.4 OAuth 2.1 + DCR

```go
// internal/server/oauth/server.go
import "github.com/ory/fosite/compose"
import "github.com/ory/fosite/storage"

// 暴露：
// - /.well-known/oauth-authorization-server（RFC 8414）
// - /.well-known/oauth-protected-resource（RFC 9728）
// - /oauth/register（DCR, RFC 7591）
// - /oauth/authorize
// - /oauth/token
// - /oauth/revoke
```

#### 7.9.5 身份解析

`X-OpenViking-Account/User/Actor-Peer` header → `RequestContext` → `context.Context` 传播（对应 `identity.py` + `_IdentityASGIMiddleware`）。

```go
// internal/server/identity/identity.go
type Identity struct {
    Account   string
    User      string
    ActorPeer string
}

func WithIdentity(ctx context.Context, id Identity) context.Context { ... }
func FromContext(ctx context.Context) (Identity, bool) { ... }
```

### 7.10 cli — 命令行与 TUI

#### 7.10.1 架构

| Rust 模块 | Go 模块 | 依赖 |
|---------|---------|------|
| `ov_cli/src/main.rs` | `cmd/ov/main.go` | cobra |
| `ov_cli/src/client.rs` | `internal/cli/client.go` | 复用 `pkg/sdk` |
| `ov_cli/src/commands/*` | `internal/cli/commands/*` | cobra |
| `ov_cli/src/config_wizard/` | `internal/cli/wizard/` | bubbletea + huh |
| `ov_cli/src/tui/` | `internal/cli/tui/` | bubbletea + lipgloss |
| `ov_cli/src/i18n.rs` | `pkg/i18n/` | `nicksnyder/go-i18n` |

#### 7.10.2 命令清单（与现状对齐）

```
ov init                    # 配置向导
ov add-resource PATH       # 添加资源
ov ls [PATH]               # 列出
ov find QUERY              # 检索
ov search QUERY            # 语义搜索
ov read PATH               # 读
ov skills list             # 技能
ov session create/commit   # 会话
ov task list               # 任务
ov observer list           # 观察者
ov snapshot create/restore # 快照
ov privacy set/get         # 隐私
ov crypto encrypt/decrypt  # 加密
ov admin account/user/key  # 管理
ov doctor                  # 诊断
ov tui                     # 进入 TUI 模式
```

### 7.11 bot — vikingbot 多渠道 Agent

#### 7.11.1 架构

| Python 模块 | Go 模块 | 依赖 |
|-----------|---------|------|
| `vikingbot/__main__.py` | `cmd/vikingbot/main.go` | cobra |
| `vikingbot/cli/` | `internal/bot/cli/` | cobra |
| `vikingbot/config/` | `internal/bot/config/` | viper |
| `vikingbot/providers/` | `internal/bot/providers/` | eino + go-openai |
| `vikingbot/channels/` | `internal/bot/channels/` | 各渠道 SDK |
| `vikingbot/bus/` | `internal/bot/bus/` | nhooyr/websocket + asynq |
| `vikingbot/sandbox/` | `internal/bot/sandbox/` | exec + container |
| `vikingbot/session/` | `internal/bot/session/` | 复用 internal/session |
| `vikingbot/cron/` | `internal/bot/cron/` | gocron |
| `vikingbot/heartbeat/` | `internal/bot/heartbeat/` | 自研 |
| `vikingbot/hooks/` | `internal/bot/hooks/` | 自研 |
| `vikingbot/integrations/langfuse.py` | `internal/bot/integrations/langfuse.go` | HTTP |
| `vikingbot/observability/` | `internal/bot/observability/` | OTel |
| `vikingbot/console/` | `internal/bot/console/` | bubbletea |
| `vikingbot/openviking_mount/` | `internal/bot/ovmount/` | 复用 pkg/sdk |
| `vikingbot/agent/` | `internal/bot/agent/` | eino 或 langchaingo |

#### 7.11.2 渠道 SDK

| 渠道 | Python | Go | 备注 |
|-----|--------|----|----|
| Telegram | python-telegram-bot | `go-telegram/bot` | 主流 |
| 飞书 | lark-oapi | `larksuite/oapi-sdk-go/v3` | 官方 |
| DingTalk | dingtalk-stream | **`open-dingtalk/dingtalk-stream-sdk-go`** | 官方 stream SDK |
| Slack | slack-sdk | `slack-go/slack` | 主流 |
| QQ | qq-botpy | HTTP（无成熟 Go SDK，自写） | 风险 [12.4](#124-qq-渠道无成熟-go-sdk) |
| WebSocket | python-socketio | `coder/websocket` 或 `gorilla/websocket` | 主流 |
| Sandbox | opensandbox | exec + containerd-shim 或外挂 sidecar | 见 [12.5](#125-sandbox-集成) |

### 7.12 web-studio — 前端集成

| 项 | 现状 | Go 重写 |
|----|-----|---------|
| 构建产物 | Vite SPA | 不变 |
| 后端托管 | FastAPI 静态目录 | Go `embed.FS` 嵌入 `web-studio/dist` |
| 路由 | `/studio/*` | 不变 |
| API 调用 | `/api/v1/*` | 不变 |
| 实时数据 | polling | 可选 SSE/WebSocket（nhooyr） |

构建流程：

```makefile
web-studio: ## 构建 Web Studio
	cd web-studio && pnpm install && pnpm build

build: web-studio ## 编译 Go 二进制（含 embed 前端）
	go build -o bin/openviking-server ./cmd/openviking-server
	go build -o bin/ov ./cmd/ov
	go build -o bin/vikingbot ./cmd/vikingbot
```

### 7.13 observability — 可观测性

#### 7.13.1 OTel

```go
// internal/observability/tracer.go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
    "go.opentelemetry.io/otel/sdk/trace"
)

func InitTracer(cfg Config) (func(), error) {
    var exp trace.SpanExporter
    switch cfg.Exporter {
    case "otlp-grpc": exp, _ = otlptracegrpc.New(ctx, ...)
    case "otlp-http": exp, _ = otlptracehttp.New(ctx, ...)
    case "memory":   exp = memory.NewExporter()
    }
    tp := trace.NewTracerProvider(
        trace.WithBatcher(exp),
        trace.WithResource(resource.NewWithAttributes(
            semconv.ServiceName("openviking-server"),
            semconv.ServiceVersion(version),
        )),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.TraceContext())
    return tp.Shutdown, nil
}
```

#### 7.13.2 Prometheus 指标

| 指标 | 类型 | labels |
|-----|------|--------|
| `openviking_http_requests_total` | counter | method, path, status |
| `openviking_http_request_duration_seconds` | histogram | method, path |
| `openviking_retrieve_hits_total` | counter | collection, level |
| `openviking_retrieve_latency_seconds` | histogram | collection, level |
| `openviking_embed_tokens_total` | counter | provider, model |
| `openviking_rerank_calls_total` | counter | provider |
| `openviking_cache_hits_total` / `_misses_total` | counter | backend |
| `openviking_queue_depth` | gauge | queue, state |
| `openviking_session_active` | gauge | account |
| `openviking_resource_count` | gauge | account, type |

#### 7.13.3 Usage Audit

对应 `observability/usage_audit/`：记录每个 embed/rerank/vlm 调用的 token 用量，写入 SQLite（modernc.org/sqlite）+ 暴露 `/api/v1/stats`。

### 7.14 工程基础设施

#### 7.14.1 优雅停机

所有 `cmd/` 入口均实现 signal-aware 优雅停机，按依赖逆序关闭资源：

```go
// cmd/openviking-server/main.go
func run() error {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    app, cleanup, err := buildApp(cfg)
    if err != nil { return err }
    defer cleanup()

    srv := &http.Server{Addr: cfg.Server.Addr, Handler: app.Router()}
    errCh := make(chan error, 1)
    go func() { errCh <- srv.ListenAndServe() }()

    select {
    case <-ctx.Done():
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        srv.Shutdown(shutdownCtx)           // 1. 停止接收新请求，等待活跃请求完成
        app.AsynqServer().Shutdown()        // 2. 等待队列任务完成
        app.TracerShutdown()(shutdownCtx)   // 3. flush OTel spans
        return nil
    case err := <-errCh:
        return err
    }
}
```

**停机顺序**：HTTP listener → asynq worker → OTel exporter → 数据库连接池 → Redis。反向依赖确保不丢请求和 trace。

#### 7.14.2 错误处理

采用 Go 标准错误模式，业务错误码与 HTTP 状态码通过中间件统一映射：

```go
// internal/domain/errors.go
var (
    ErrNotFound       = errors.New("resource not found")
    ErrUnauthorized   = errors.New("unauthorized")
    ErrForbidden      = errors.New("forbidden")
    ErrConflict       = errors.New("conflict")
    ErrQuotaExceeded  = errors.New("quota exceeded")
)

type AppError struct {
    Code    string // 业务错误码，对齐 Python 版 error_mapping
    Status  int    // HTTP 状态码
    Err     error
}

func (e *AppError) Error() string { return e.Err.Error() }
func (e *AppError) Unwrap() error { return e.Err }
```

**规则**：

- 用 `fmt.Errorf("context: %w", err)` 包装上下文
- 用 `errors.Is` / `errors.As` 做错误判断，不做字符串匹配
- 在 Gin recovery middleware 统一将 `AppError` 映射为 JSON response
- 内部函数只返回 `error`，不做 HTTP 状态码映射

#### 7.14.3 依赖注入

项目规模适合**手动构造函数注入**，不引入 Wire/Fx 等框架，降低编译期复杂度：

```go
// cmd/openviking-server/app.go
func buildApp(cfg *config.Config) (*server.App, func(), error) {
    // 基础设施层
    dbPool, err := storage.NewPgPool(ctx, cfg.Database)
    if err != nil { return nil, nil, err }
    cache := ragfs.NewRedisCache(cfg.Redis)

    // 存储层
    fs := ragfs.NewMountableFS(cfg.RAGFS, cache)
    vdb := vectordb.NewManager(cfg.VectorDB)

    // 模型层
    embedder := models.NewEmbedder(cfg.Embedder)
    reranker := models.NewReranker(cfg.Rerank)
    vlm := models.NewVLM(cfg.VLM)

    // 业务层
    retriever := retrieve.NewHierarchical(vdb, embedder, reranker, vlm)
    sessionMgr := session.NewManager(vlm, fs)
    svc := service.New(fs, vdb, retriever, sessionMgr)

    // 传输层
    app := server.NewApp(cfg.Server, svc)
    cleanup := func() {
        dbPool.Close()
        cache.Close()
    }
    return app, cleanup, nil
}
```

#### 7.14.4 限流与熔断

| 机制 | 库 | 用途 |
|-----|----|----|
| 本地限流 | `golang.org/x/time/rate` | API 限流中间件（per-account / global） |
| 熔断 | `sony/gobreaker` | 外部 API 调用（embedding/rerank/VLM） |
| 重试 | `cenkalti/backoff/v4` | 可重试的外部调用（指数退避 + jitter） |

```go
// internal/server/middleware/ratelimit.go
func RateLimitMiddleware(rps float64, burst int) gin.HandlerFunc {
    limiter := rate.NewLimiter(rate.Limit(rps), burst)
    return func(c *gin.Context) {
        if !limiter.Allow() {
            c.AbortWithStatusJSON(429, gin.H{"error": "rate limit exceeded"})
            return
        }
        c.Next()
    }
}

// internal/models/embedder/openai.go
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
    var result [][]float32
    err := e.breaker.Execute(func() error {
        var execErr error
        result, execErr = e.doEmbed(ctx, texts)
        return execErr
    })
    return result, err
}
```

#### 7.14.5 Context 传播

所有函数第一参数为 `context.Context`，传播链路：

```
Gin.Context → context.WithValue(identity) → service → repository/adapter
                                           → OTel span（自动关联 trace）
                                           → slog 结构化字段（requestID/account）
```

**规则**：

- 禁止使用 `context.Background()` 跨越已有 context
- 所有阻塞调用必须响应 `ctx.Done()`
- 使用 `context.AfterFunc`（Go 1.21+）做资源清理
- 跨 goroutine 传递时使用 `context.WithoutCancel` 避免父 cancel 影响后台任务

#### 7.14.6 错误码与 HTTP 状态码映射

业务错误码对齐 Python `openviking/server/error_mapping.py`，统一通过 Gin 中间件映射为 HTTP 响应。

| 业务错误码 | HTTP 状态 | ErrXxx | 含义 |
|-----------|---------|--------|------|
| `RESOURCE_NOT_FOUND` | 404 | `ErrNotFound` | 资源/会话/技能不存在 |
| `ACCOUNT_NOT_FOUND` | 404 | `ErrAccountNotFound` | 账户不存在 |
| `UNAUTHORIZED` | 401 | `ErrUnauthorized` | 未认证 / API key 无效 |
| `FORBIDDEN` | 403 | `ErrForbidden` | 无权限访问该 account/user 资源 |
| `ACCOUNT_DISABLED` | 403 | `ErrAccountDisabled` | 账户已禁用 |
| `CONFLICT` | 409 | `ErrConflict` | 版本冲突 / 路径已存在 |
| `VALIDATION_FAILED` | 422 | `ErrValidation` | 请求体校验失败（validator 返回） |
| `QUOTA_EXCEEDED` | 429 | `ErrQuotaExceeded` | 配额超限（token/存储/请求频率） |
| `RATE_LIMITED` | 429 | `ErrRateLimited` | 限流（区别于配额） |
| `PAYLOAD_TOO_LARGE` | 413 | `ErrPayloadTooLarge` | 上传文件超限 |
| `UNSUPPORTED_MEDIA_TYPE` | 415 | `ErrUnsupportedMedia` | 文件类型不支持解析 |
| `PARSE_FAILED` | 422 | `ErrParseFailed` | 文档解析失败 |
| `EMBED_FAILED` | 502 | `ErrEmbedFailed` | embedding provider 错误 |
| `RERANK_FAILED` | 502 | `ErrRerankFailed` | rerank provider 错误 |
| `VLM_FAILED` | 502 | `ErrVLMFailed` | VLM provider 错误 |
| `VECTORDB_ERROR` | 500 | `ErrVectorDB` | 向量库内部错误 |
| `RAGFS_ERROR` | 500 | `ErrRAGFS` | 文件系统范式存储错误 |
| `QUEUE_ERROR` | 500 | `ErrQueue` | 队列调度错误 |
| `INTERNAL_ERROR` | 500 | `ErrInternal` | 未分类内部错误 |
| `SERVICE_UNAVAILABLE` | 503 | `ErrUnavailable` | 维护模式 / 依赖不可用 |

**响应体格式**（与 Python 版一致）：

```json
{
  "error": {
    "code": "RESOURCE_NOT_FOUND",
    "message": "resource not found: viking://acct/res/abc",
    "request_id": "req_01HXYZ...",
    "details": { "uri": "viking://acct/res/abc" }
  }
}
```

**Gin 中间件实现**：

```go
// internal/server/middleware/error.go
func ErrorMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Next()
        if len(c.Errors) == 0 { return }
        err := c.Errors.Last().Err
        var appErr *domain.AppError
        switch {
        case errors.As(err, &appErr):
            c.JSON(appErr.Status, gin.H{"error": appErr.ToResponse(c)})
        default:
            c.JSON(500, gin.H{"error": gin.H{
                "code": "INTERNAL_ERROR",
                "message": err.Error(),
                "request_id": c.GetString("request_id"),
            }})
        }
    }
}
```

### 7.15 数据模型与 `viking://` URI

#### 7.15.1 核心领域模型

**Resource**：

```go
// internal/domain/resource.go
type Resource struct {
    URI         string            `json:"uri"`          // viking://acct/...
    Type        ResourceType      `json:"type"`         // file / dir / url / memory / skill / session
    Name        string            `json:"name"`
    Parent      string            `json:"parent,omitempty"`
    MimeType    string            `json:"mime_type,omitempty"`
    Size        int64             `json:"size,omitempty"`
    Hash        string            `json:"hash,omitempty"`     // xxhash
    CreatedAt   time.Time         `json:"created_at"`
    ModifiedAt  time.Time         `json:"modified_at"`
    Owner       Identifier       `json:"owner"`             // account/user/actor_peer
    Metadata    map[string]any   `json:"metadata,omitempty"`
    Layers      LayerInfo         `json:"layers"`            // L0/L1/L2 状态
}

type LayerInfo struct {
    HasAbstract  bool   `json:"has_abstract"`
    HasOverview  bool   `json:"has_overview"`
    ChunkCount   int    `json:"chunk_count"`
    FallbackL1   bool   `json:"fallback_l1,omitempty"`   // L1 生成失败降级
}
```

**Session**：

```go
// internal/domain/session.go
type Session struct {
    ID          string            `json:"id"`            // ulid
    Account     string            `json:"account"`
    User        string            `json:"user"`
    Peer        string            `json:"peer"`
    Status      SessionStatus     `json:"status"`        // active / committed / archived
    Turns       []Turn            `json:"turns"`
    Summary     string            `json:"summary,omitempty"`
    Memory      []ExtractedMemory `json:"memory,omitempty"`
    CreatedAt   time.Time         `json:"created_at"`
    CommittedAt *time.Time        `json:"committed_at,omitempty"`
    TokenUsage  TokenUsage        `json:"token_usage"`
}

type Turn struct {
    ID        string         `json:"id"`
    Role      TurnRole       `json:"role"`        // user / assistant / tool
    Content   string         `json:"content"`
    ToolCalls []ToolCall     `json:"tool_calls,omitempty"`
    Tokens    int            `json:"tokens"`
    CreatedAt time.Time      `json:"created_at"`
}
```

**Skill**：

```go
// internal/domain/skill.go
type Skill struct {
    URI         string         `json:"uri"`           // viking://agent/skills/{name}
    Name        string         `json:"name"`
    Level       int            `json:"level"`         // 0=根, 1=子, ...
    Description string         `json:"description"`
    Trigger     string         `json:"trigger"`       // 自然语言触发条件
    Steps       []SkillStep    `json:"steps"`
    Files       []string       `json:"files"`         // 关联资源 URI
    Embedding   []float32      `json:"-"`             // 语义检索用
    Metadata    map[string]any `json:"metadata,omitempty"`
}
```

#### 7.15.2 `viking://` URI 体系

URI 是多租户隔离的核心。**Go 版与 Python 版字符串格式 100% 兼容**。

**URI 格式**：

```
viking://[account]/[namespace]/[path...][?version=xxx]

示例：
viking://acct_001/resources/docs/readme.md
viking://acct_001/sessions/01HXYZ...
viking://acct_001/memory/fact/01HABC...
viking://agent/skills/code-review          # 账户共享技能
viking://user_001/skills/my-skill          # 用户私有技能
```

**命名空间**：

| Namespace | 含义 | 多租户范围 |
|-----------|-----|----------|
| `resources` | 用户上传的资源 | account + user |
| `sessions` | Agent 会话 | account + user + peer |
| `memory` | 长期记忆 | account + user |
| `skills` | 技能 | account-shared 或 user-private |
| `relations` | 资源关系图 | account |
| `snapshots` | 快照 | account |

**解析规则**：

```go
// pkg/vikinguri/uri.go
type URI struct {
    Account   string
    Namespace Namespace
    Path      []string
    Version   string
}

func Parse(s string) (*URI, error) {
    if !strings.HasPrefix(s, "viking://") {
        return nil, ErrInvalidURI
    }
    rest := strings.TrimPrefix(s, "viking://")
    parts := strings.SplitN(rest, "/", 2)
    // 特例：viking://agent/skills/... 与 viking://user_xxx/skills/...
    // account 段可能是 "agent" 或 "user_xxx"（共享 vs 私有）
    ...
}
```

**多租户隔离**：

| 层级 | 机制 |
|-----|------|
| 路径前缀 | ragfs `MountableFS` 按 `viking://acct_001/` 挂载到不同物理目录 / S3 prefix |
| 权限 | `Identity` 从 HTTP header 解析后，所有 ragfs/vectordb 调用强制带 account 维度 |
| 向量库 | 每个 account 一个独立 collection（或 qdrant payload 过滤） |
| 配额 | per-account rate limiter（`golang.org/x/time/rate`） |

### 7.16 ovpack 离线格式

ovpack 是 OpenViking 的离线打包格式，用于快照、迁移、跨环境数据传递。**Go 版与 Python `storage/ovpack/` 二进制兼容**（zip + manifest）。

#### 7.16.1 文件结构

```
my-snapshot.ovpack                      # 实际是 zip
├── manifest.json                       # 清单（schema 见下）
├── resources/                          # 资源原文
│   ├── docs/
│   │   └── readme.md
│   └── images/
│       └── logo.png
├── layers/                             # L0/L1 隐藏文件
│   ├── docs/
│   │   ├── readme.md.abstract
│   │   └── readme.md.overview
│   └── ...
├── vectors/
│   ├── chunks/
│   │   └── docs/
│   │       └── readme.md.vec.jsonl     # 每行一条向量
│   ├── abstract/
│   └── overview/
├── sessions/
│   └── 01HXYZ...json
├── memory/
│   └── 01HABC...json
├── skills/
│   └── code-review.json
└── relations/
    └── graph.json
```

#### 7.16.2 manifest schema

```json
{
  "format": "ovpack",
  "version": "1.0",
  "exported_at": "2026-07-04T12:00:00Z",
  "exporter": "openviking-go/1.0.0",
  "source": {
    "account": "acct_001",
    "base_uri": "viking://acct_001/"
  },
  "contents": {
    "resources":   { "count": 142, "bytes": 52428800 },
    "layers":      { "abstract": 142, "overview": 138, "fallback_l1": 4 },
    "vectors":     { "chunks": 12450, "abstract": 142, "overview": 138, "dim": 1536 },
    "sessions":    { "count": 12 },
    "memory":      { "count": 87 },
    "skills":      { "count": 5 },
    "relations":   { "edges": 230 }
  },
  "embedder":     { "provider": "openai", "model": "text-embedding-3-large", "dim": 1536 },
  "vlm":          { "provider": "volcengine", "model": "doubao-seed-2-0-lite-260428" },
  "checksum": {
    "algorithm": "xxhash64",
    "manifest":  "...",      // manifest.json 自身的 hash
    "archive":   "..."       // 整个 zip 内容（除 manifest）的 hash
  }
}
```

#### 7.16.3 导入导出协议

**导出**（`POST /api/v1/pack/export`）：

1. 校验 account 权限
2. 收集资源清单 + 向量 + 元数据
3. 写入 zip 流（避免双倍内存）
4. 计算 checksum
5. 返回 `application/octet-stream`

**导入**（`POST /api/v1/pack/import`）：

1. 校验 manifest schema + checksum
2. 检查 embedder/vlm 兼容性（dim 必须一致；vlm 不一致则跳过 L0/L1 重生成）
3. **两阶段提交**：
   - 阶段 A：解压到临时目录，逐项校验
   - 阶段 B：原子提交（rename 到目标路径 + upsert 向量 + 写 memory）
4. 失败回滚：删除已写入的临时文件 + asynq 任务补偿已 upsert 的向量

**Go 实现**：

```go
// pkg/ovpack/writer.go
type Writer struct {
    zw       *zip.Writer
    manifest Manifest
    hash     xxhash64.Digest
}

func (w *Writer) WriteResource(uri string, content []byte) error {
    path := uriToPath(uri)              // viking://acct/x.md → resources/x.md
    f, _ := w.zw.Create(path)
    _, err := f.Write(content)
    w.hash.Write(content)
    w.manifest.Contents.Resources.Count++
    w.manifest.Contents.Resources.Bytes += int64(len(content))
    return err
}

func (w *Writer) Close() error {
    // 写 manifest.json（带 checksum）
    w.manifest.Checksum.Manifest = w.hash.SumHex64(nil)
    data, _ := sonic.MarshalIndent(w.manifest, "", "  ")
    f, _ := w.zw.Create("manifest.json")
    f.Write(data)
    return w.zw.Close()
}
```

**兼容性**：

- Go 版导出的 ovpack 必须能被 Python 版导入，反之亦然
- `format=ovpack, version=1.0` 锁定，schema 变更走版本号
- 集成测试覆盖双向导入导出（详见 [9.3.2](#932-集成矩阵)）

---

## 8. 实施方案

### 8.1 阶段划分

| 阶段 | 目标 | 关键产物 | 验收 | 预估周期 |
|-----|-----|---------|------|---------|
| P0 骨架 | 仓库初始化、go.mod、CI | 空骨架可编译 | `make build` 通过 | 1 周 |
| P1 配置与领域模型 | viper config、domain models | config + domain 包 | 单测覆盖 | 1 周 |
| P2 HTTP API 骨架 | gin + middleware + 路由注册 | 24 个 router 框架 | curl 健康检查 | 2 周 |
| P3 ragfs 核心 | FileSystem 接口 + localfs/memfs/s3fs | 基础 backend | 单测 + 集成测 | 3 周 |
| P4 vectordb 适配 | qdrant + opengauss + local | 多后端可切换 | 集成测 | 2 周 |
| P5 queuefs | DAG + asynq | embedding pipeline | 集成测 | 2 周 |
| P6 ingest + parse | sources + accessor + parser | 资源接入闭环 | E2E：add-resource → 检索 | 3 周 |
| P7 retrieve | 分层检索 + intent + rerank | 检索 API 可用 | 检索质量对照 | 3 周 |
| P8 session | 压缩 v2/v3 + memory + skill | session 闭环 | E2E：session commit → 记忆 | 4 周 |
| P9 MCP + OAuth | mcp-go + fosite | MCP 工具全可用 | Claude Code 对接 | 2 周 |
| P10 CLI + TUI | cobra + bubbletea | ov 命令完备 | 现有 SDK test 通过 | 2 周 |
| P11 bot 重写 | providers + 5 渠道 | vikingbot 可启动 | 至少 1 渠道 E2E | 3 周 |
| P12 web-studio embed | embed + 路由 | 前端可访问 | 浏览器回归 | 1 周 |
| P13 observability | OTel + metrics + audit | 可观测 | Grafana 板可用 | 2 周 |
| P14 迁移工具 | ovpack 导入导出 + vectordb migration | 现状数据可迁入 | 真实数据迁移 | 2 周 |
| P15 性能优化 | benchmark + profiling | 性能达标 | 与 Rust 版对照 | 2 周 |
| P16 文档与发布 | docs/Deploy/Release | 1.0 发布 | 灰度上线 | 1 周 |

**总周期：约 36 周（8-9 个月，2-3 人并行）**。

### 8.2 里程碑

| 里程碑 | 时间点 | 标志 |
|-------|-------|------|
| M1 内部演示 | P7 完成 | 可用 Go 版进行检索 demo |
| M2 MVP | P10 完成 | CLI + 服务可用，对接现有 SDK |
| M3 Beta | P13 完成 | Bot + 可观测性完整 |
| M4 RC | P15 完成 | 性能达标，文档齐全 |
| M5 1.0 | P16 完成 | 灰度上线 |

### 8.3 并行策略

| 可并行模块 | 前置 |
|-----------|------|
| ragfs / vectordb / models / parse | domain（P1） |
| ingest / retrieve / session | ragfs + vectordb + models（P3-P5） |
| server / cli / bot | service 层（P6-P8） |
| observability / migrate / docs | 任意阶段 |

### 8.4 团队建议

| 角色 | 人数 | 主要负责 |
|-----|-----|---------|
| 后端 Go 工程师 | 2 | ragfs/vectordb/ingest/retrieve/session |
| 平台工程师 | 1 | server/cli/observability/deploy |
| Bot/集成工程师 | 1 | vikingbot/channels/integrations |
| QA | 1 | 测试方案执行、E2E、benchmark |
| 文档 | 0.5 | docs/examples/迁移指南 |

---

## 9. 测试方案

### 9.1 测试金字塔

```
                    /\
                   /E2E\        ←  少量，playwright-go + 真实依赖
                  /------\
                 / 集成  \      ←  testcontainers-go（redis/qdrant/postgres/minio）
                /----------\
               /   单元    \    ←  大量，testing + testify + mock
              /--------------\
```

### 9.2 单元测试

| 模块 | 工具 | 覆盖目标 |
|-----|-----|---------|
| domain | testing + testify | 90% |
| ragfs | testing + 内存 mock backend | 85% |
| vectordb adapters | testify/mock + sqlmock | 80% |
| queuefs | asynq in-memory | 80% |
| ingest sources | 文件 fixture | 85% |
| parse parsers | 文件 fixture（每种格式） | 80% |
| retrieve | mock embedder/rerank/vlm | 85% |
| session | mock vlm | 80% |
| models | httpmock | 85% |
| server routers | httptest + gin | 80% |
| bot channels | mock channel SDK | 70% |
| observability | memory exporter | 80% |

**约定**：

- 文件命名 `xxx_test.go`，与被测文件同包
- 表驱动测试优先（`tests := []struct{...}`)
- 用 `testify/require` 做断言，`testify/assert` 做非关键断言
- mock 用 `testify/mock`，避免 mockgen 除非接口复杂

### 9.3 集成测试

#### 9.3.1 testcontainers-go

```go
// tests/integration/redis_test.go
func TestRedisCache(t *testing.T) {
    ctx := context.Background()
    ctr, err := redis.Run(ctx, "redis:7-alpine")
    require.NoError(t, err)
    t.Cleanup(func() { ctr.Terminate(ctx) })

    host, _ := ctr.Endpoint(ctx, "")
    cache := ragfs.NewRedisCache(host)
    // ...
}
```

#### 9.3.2 集成矩阵

| 后端 | 镜像 | 覆盖 |
|-----|------|-----|
| Redis | `redis:7-alpine` | cache、asynq queue |
| Qdrant | `qdrant/qdrant:v1.x` | 向量 |
| Postgres+pgvector | `pgvector/pgvector:pg16` | opengauss 后端 |
| MinIO | `minio/minio` | S3 backend |
| SQLite | in-process | local KV、cursor_store、audit |

### 9.4 E2E 测试

#### 9.4.1 API E2E

`tests/e2e/api/` 用真实 `openviking-server` 二进制 + testcontainers 后端，覆盖：

- 资源接入 → 解析 → embedding → 检索 全链路
- Session commit → 记忆提取
- MCP 工具调用
- WebDAV 上传/下载
- ovpack 导出/导入

#### 9.4.2 SDK E2E

直接复用现有 `sdk/go` 与 `sdk/python` 测试套件（**API 兼容验收**）。

#### 9.4.3 Bot E2E

`tests/e2e/bot/` 用 mock channel server 模拟 Telegram/Slack 等，验证 vikingbot 接收消息 → 调用 OpenViking → 回复。

#### 9.4.4 Web Studio E2E

`tests/e2e/web/` 用 `playwright-go` 驱动浏览器，覆盖：

- 资源浏览
- 检索 playground
- 多 Agent hub
- 控制台

### 9.5 评估测试

#### 9.5.1 RAGAS 等价框架

Go 无 ragas 等价库，自研 `internal/eval/` 框架：

```go
// internal/eval/metrics.go
type Metric interface {
    Name() string
    Calculate(ctx context.Context, sample Sample) (float64, error)
}

// 内置指标（与 ragas 对齐）
type Faithfulness struct{ vlm vlm.VLM }       // 忠实度
type AnswerRelevancy struct{ vlm vlm.VLM }    // 答案相关性
type ContextPrecision struct{ vlm vlm.VLM }   // 上下文精确度
type ContextRecall struct{ vlm vlm.VLM }      // 上下文召回
type ContextEntityRecall struct{ vlm vlm.VLM } // 实体召回
```

#### 9.5.2 benchmark 对照

`benchmark/` 目录提供：

- 与现状 Rust+Python 版同数据集对照（User Memory / Agent Memory / Knowledge Base QA，参考 README `Evaluation Highlights`）
- `go test -bench` + `benchstat` 输出统计
- 检索 P50/P95/P99 延迟、QPS、内存

### 9.6 性能与回归

| 维度 | 工具 | 频率 |
|-----|-----|------|
| 微基准 | `go test -bench` | 每次提交 |
| 集成基准 | `benchmark/` 脚本 | 每周 |
| 性能回归 | `benchstat` 对照 main | 每次提交（CI） |
| pprof | `net/http/pprof` | 按需 |
| 内存 | `runtime.MemProfile` | 每周 |

### 9.7 CI 流水线

```yaml
# .github/workflows/go.yml（节选）
jobs:
  lint:
    runs-on: ubuntu-latest
    steps: [setup-go, golangci-lint]
  unit:
    runs-on: ubuntu-latest
    steps: [setup-go, go-test -race -cover]
  integration:
    services: [redis, qdrant, postgres, minio]
    steps: [setup-go, go-test -tags=integration]
  e2e:
    steps: [build-server, build-bot, playwright, go-test -tags=e2e]
  benchmark:
    if: branch == main
    steps: [go-test -bench, benchstat-compare]
  security:
    steps: [govulncheck, trivy fs]
```

#### 9.7.1 矩阵策略

| Job | OS 矩阵 | Go 矩阵 | 服务容器 |
|-----|---------|---------|---------|
| lint | ubuntu-latest | 1.24.x | — |
| unit | ubuntu-latest, macos-latest, windows-latest | 1.24.x | — |
| integration | ubuntu-latest | 1.24.x | redis, qdrant, postgres+pgvector, minio |
| e2e | ubuntu-latest | 1.24.x | redis, qdrant, minio |
| cross-build | ubuntu-latest | 1.24.x | — |
| benchmark | ubuntu-latest | 1.24.x | redis, qdrant |

**cross-build 矩阵**（产物矩阵）：

```yaml
strategy:
  matrix:
    target:
      - linux/amd64
      - linux/arm64
      - darwin/amd64
      - darwin/arm64
      - windows/amd64
steps:
  - run: GOOS=${{matrix.os}} GOARCH=${{matrix.arch}} CGO_ENABLED=0 \
         go build -ldflags="-s -w" -o dist/ov-${{matrix.os}}-${{matrix.arch}} ./cmd/ov
```

#### 9.7.2 缓存策略

| 缓存 | 工具 | key |
|-----|------|-----|
| Go build cache | `actions/setup-go@v5`（内置 cache） | `go-mod-${{hashFiles('**/go.sum')}}` |
| Go mod cache | `actions/cache@v4` | `go-mod-${{hashFiles('**/go.sum')}}` |
| Web Studio npm | `actions/setup-node@v4` cache: pnpm | `pnpm-${{hashFiles('web-studio/pnpm-lock.yaml')}}` |
| Playwright browsers | `playwright-community/playwright-go` cache | `playwright-${{matrix.os}}` |
| Docker layer | `docker/build-push-action@v5` cache-from: gha | `docker-${{hashFiles('Dockerfile')}}` |

#### 9.7.3 发布产物

| 产物 | 形式 | 发布渠道 | 命名 |
|-----|------|---------|------|
| Linux 二进制 | tar.gz | GitHub Release | `openviking-server-linux-amd64.tar.gz` |
| macOS 二进制 | tar.gz | GitHub Release | `openviking-server-darwin-arm64.tar.gz` |
| Windows 二进制 | zip | GitHub Release | `openviking-server-windows-amd64.zip` |
| Docker 镜像 | multi-arch | ghcr.io / Docker Hub | `ghcr.io/volcengine/openviking-server:{version}` |
| Helm Chart | tar.gz | GitHub Release / OCI registry | `openviking-{version}.tgz` |
| deb/rpm | package | GitHub Release（Linux only） | `openviking-server_{version}_amd64.deb` |
| Checksums | txt | GitHub Release | `checksums.txt`（SHA256） |
| SBOM | spdx-json | GitHub Release | `sbom.spdx.json`（`syft` 生成） |

**发布流水线**（tag `v1.0.0` 触发）：

```yaml
release:
  if: startsWith(github.ref, 'refs/tags/v')
  needs: [lint, unit, integration, e2e, cross-build, security]
  steps:
    - name: goreleaser        # 自动生成 cross-build + changelog + GitHub Release
    - name: docker/build-push # multi-arch (amd64 + arm64) push to ghcr.io
    - name: helm package      # helm lint + package + push to OCI
    - name: sbom              # syft generate SBOM
    - name: cosign sign       # 镜像签名（cosign，keyless）
```

#### 9.7.4 质量门禁

| 门禁 | 阈值 | 阻断条件 |
|-----|------|---------|
| 单元覆盖率 | ≥ 80% | 低于阈值阻断 PR |
| lint | golangci-lint 0 error | 任何 error 阻断 |
| vuln | govulncheck 0 high | 任何 high 阻断 |
| 性能回归 | benchstat > 10% | main 分支回归阻断 |
| E2E | 100% 通过 | 任何 fail 阻断 |

---

## 10. 部署与运维

### 10.1 部署形态

| 形态 | 适用 | 配置 |
|-----|-----|------|
| 单二进制 | 个人/小团队 | `openviking-server` + SQLite + local vectordb |
| Server + Worker | 中型 | `openviking-server` + `asynq worker` + Redis + Qdrant |
| K8s 集群 | 大型/多租户 | Helm chart：server/worker/bot/statefulset（qdrant） |

### 10.2 Docker

```dockerfile
# Dockerfile
FROM node:20-alpine AS web-builder
WORKDIR /app/web-studio
COPY web-studio/ .
RUN pnpm install && pnpm build

FROM golang:1.24-alpine AS go-builder
WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web-studio/dist ./web-studio/dist
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/openviking-server ./cmd/openviking-server
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/ov ./cmd/ov
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/vikingbot ./cmd/vikingbot

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go-builder /out/* /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["openviking-server"]
```

### 10.3 Helm Chart

更新 `deploy/helm/`：

```yaml
# deploy/helm/openviking/values.yaml
server:
  replicas: 2
  resources:
    limits: { cpu: 2, memory: 2Gi }
worker:
  replicas: 4
  resources:
    limits: { cpu: 4, memory: 4Gi }
bot:
  enabled: false
qdrant:
  enabled: true
  persistence: { size: 100Gi }
redis:
  enabled: true
observability:
  otel:
    endpoint: otel-collector:4317
  prometheus:
    enabled: true
```

### 10.4 配置兼容

`ov.conf.example` 与 `ovcli.conf.example` 字段保持兼容，YAML schema 不变。Go 端用 viper 加载：

```go
// internal/config/config.go
type Config struct {
    Server   ServerConfig   `mapstructure:"server"`
    VLM      VLMConfig      `mapstructure:"vlm"`
    Embedder EmbedderConfig `mapstructure:"embedder"`
    Rerank   RerankConfig   `mapstructure:"rerank"`
    VectorDB VectorDBConfig `mapstructure:"vectordb"`
    RAGFS    RAGFSConfig    `mapstructure:"ragfs"`
    Auth     AuthConfig     `mapstructure:"auth"`
    OAuth    OAuthConfig    `mapstructure:"oauth"`
    OTEL     OTELConfig     `mapstructure:"otel"`
    Bot      BotConfig      `mapstructure:"bot"`
}
```

#### 10.4.1 字段对照表（与 `ov.conf.example` 对齐）

| YAML 路径 | Go struct 字段 | 类型 | 默认 | 说明 |
|----------|---------------|-----|------|------|
| `server.host` | `Server.Host` | string | `0.0.0.0` | 监听地址 |
| `server.port` | `Server.Port` | int | `8080` | 监听端口 |
| `server.workers` | `Server.Workers` | int | `runtime.NumCPU()` | Gin handler 池大小（实际由 GMP 调度，仅作 hint） |
| `server.upload_dir` | `Server.UploadDir` | string | `/tmp/ov-uploads` | 临时上传目录 |
| `server.max_upload_size` | `Server.MaxUploadSize` | int64 | `1073741824` | 1GB |
| `vlm.provider` | `VLM.Provider` | string | — | openai/volcengine/litellm/codex/glm/kimi |
| `vlm.model` | `VLM.Model` | string | — | 模型名或 endpoint ID |
| `vlm.api_key` | `VLM.APIKey` | string | — | 从 `env:VLM_API_KEY` 解析 |
| `vlm.api_base` | `VLM.APIBase` | string | — | 自定义端点 |
| `vlm.temperature` | `VLM.Temperature` | float64 | `0.0` | |
| `vlm.max_retries` | `VLM.MaxRetries` | int | `2` | |
| `embedder.provider` | `Embedder.Provider` | string | — | openai/volcengine/vikingdb/dashscope/cohere/jina/minimax/voyage/gemini/litellm/local |
| `embedder.model` | `Embedder.Model` | string | — | |
| `embedder.dim` | `Embedder.Dim` | int | — | 向量维度（必须与 vectordb collection 一致） |
| `embedder.api_key` | `Embedder.APIKey` | string | — | |
| `rerank.provider` | `Rerank.Provider` | string | — | cohere/openai/volcengine/litellm |
| `rerank.model` | `Rerank.Model` | string | — | |
| `rerank.top_n` | `Rerank.TopN` | int | `5` | 默认重排数 |
| `vectordb.backend` | `VectorDB.Backend` | string | `qdrant` | local/qdrant/opengauss/volcengine/vikingdb/http |
| `vectordb.collection_prefix` | `VectorDB.CollectionPrefix` | string | `ov_` | 多租户 collection 前缀 |
| `vectordb.qdrant.url` | `VectorDB.Qdrant.URL` | string | — | qdrant gRPC/HTTP |
| `vectordb.qdrant.api_key` | `VectorDB.Qdrant.APIKey` | string | — | |
| `vectordb.opengauss.dsn` | `VectorDB.OpenGauss.DSN` | string | — | pgx 连接串 |
| `vectordb.local.path` | `VectorDB.Local.Path` | string | `./data/vectordb` | hnswlib 索引文件 |
| `ragfs.mounts` | `RAGFS.Mounts` | []MountConfig | — | 挂载点列表 |
| `ragfs.cache.provider` | `RAGFS.Cache.Provider` | string | `memory` | memory/redis |
| `ragfs.cache.redis.addr` | `RAGFS.Cache.Redis.Addr` | string | — | |
| `ragfs.multi_write.backups` | `RAGFS.MultiWrite.Backups` | []string | — | 备份 backend 名 |
| `ragfs.redirect.file_over_size` | `RAGFS.Redirect.FileOverSize` | int64 | `104857600` | 100MB |
| `auth.api_key.enabled` | `Auth.APIKey.Enabled` | bool | `true` | |
| `auth.api_key.hash_algorithm` | `Auth.APIKey.HashAlgo` | string | `argon2id` | |
| `auth.oauth.enabled` | `Auth.OAuth.Enabled` | bool | `false` | MCP OAuth 2.1 |
| `oauth.issuer` | `OAuth.Issuer` | string | — | |
| `oauth.clients` | `OAuth.Clients` | []ClientConfig | — | DCR 注册的客户端 |
| `otel.exporter` | `OTEL.Exporter` | string | `memory` | memory/otlp-grpc/otlp-http |
| `otel.endpoint` | `OTEL.Endpoint` | string | — | OTLP 收集器地址 |
| `otel.service_name` | `OTEL.ServiceName` | string | `openviking-server` | |
| `otel.sample_rate` | `OTEL.SampleRate` | float64 | `1.0` | 0-1 |
| `bot.enabled` | `Bot.Enabled` | bool | `false` | |
| `bot.channels` | `Bot.Channels` | map[string]ChannelConfig | — | telegram/feishu/dingtalk/slack/qq |

#### 10.4.2 多源加载

viper 加载顺序（后者覆盖前者）：

1. 默认值（`internal/config/defaults.go` 中的 `setDefaults`）
2. `/etc/openviking/ov.conf`（系统级）
3. `$HOME/.config/openviking/ov.conf`（用户级）
4. `$OV_CONFIG_PATH`（环境变量指向的自定义路径）
5. 环境变量（前缀 `OV_`，如 `OV_VLM_API_KEY` → `vlm.api_key`）
6. 命令行 flag（`--vlm.api_key=...`）

```go
// internal/config/loader.go
func Load(flags *pflag.FlagSet) (*Config, error) {
    v := viper.New()
    v.SetEnvPrefix("OV")
    v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
    v.AutomaticEnv()
    setDefaults(v)
    for _, path := range []string{
        "/etc/openviking/ov.conf",
        filepath.Join(homeDir(), ".config/openviking/ov.conf"),
        os.Getenv("OV_CONFIG_PATH"),
    } {
        if path != "" { v.SetConfigFile(path); v.ReadInConfig() }
    }
    v.BindPFlags(flags)
    var cfg Config
    if err := v.Unmarshal(&cfg, viper.DecodeHook(decodeHooks())); err != nil {
        return nil, err
    }
    if err := validator.New().Struct(&cfg); err != nil {
        return nil, err
    }
    return &cfg, nil
}
```

#### 10.4.3 与 Python 版的差异

| 项 | Python 版 | Go 版 | 备注 |
|---|---------|-------|-----|
| 加载源 | pydantic-settings | viper | 字段名一致 |
| 环境变量前缀 | `OPENVIKING_` | `OV_` | **行为差异**：Go 版默认 `OV_`，但保留 `OPENVIKING_` 兼容（viper 多前缀） |
| 校验 | pydantic | validator/v10 | tag 一致（`required`、`oneof`、`min`、`max`） |
| 默认值来源 | dataclass default | `setDefaults()` | 值一致 |
| 热更新 | apscheduler + reload | fsnotify + SIGHUP | 行为等价 |

### 10.5 运维工具

| 工具 | 命令 | 用途 |
|-----|------|------|
| doctor | `openviking-doctor` | 配置/连通性/模型/向量库健康检查 |
| migrate | `openviking-migrate` | ovpack 导入、vectordb 后端迁移 |
| profile | `openviking-server -profile :6060` | pprof |
| shell | `ov shell` | 交互式 ragfs shell |

---

## 11. 兼容性与迁移策略

### 11.1 API 兼容

| 维度 | 策略 |
|-----|------|
| 路径 | 24 个 router 路径与字段 100% 一致 |
| 请求/响应 schema | 用 OpenAPI 描述，与 Python 版 pydantic schema 对齐 |
| 错误码 | `internal/domain/errors.go` 与 `server/error_mapping.py` 对齐 |
| Header | `X-OpenViking-Account/User/Actor-Peer`、`X-Request-ID`、`X-Process-Time` 保持 |
| MCP 工具 | 12 个工具名称/参数/返回 schema 不变 |
| WebDAV | `webdav/resources` 路径与 PROPFIND/GET/PUT/DELETE 不变 |

### 11.2 数据兼容

| 数据 | 迁移 |
|-----|------|
| ragfs 文件 | 路径与元数据文件 schema 一致，直接读 |
| 向量库 | 通过 `openviking-migrate` 从现状导出 ovpack → 导入 |
| SQLite cursor/audit | schema 一致，直接读 |
| 配置 | YAML 字段一致 |

### 11.3 SDK 兼容

- `sdk/go` 现有代码可直接迁入 `pkg/sdk/`，无需改动调用方
- `sdk/python` 通过 HTTP 调用，server 兼容则自动兼容
- Web Studio 调用 `/api/v1/*`，server 兼容则自动兼容

### 11.4 迁移路径

| 阶段 | 现状 | Go 版 | 共存方式 |
|-----|------|-------|---------|
| Phase A | 主 | 影子 | Go 版以影子模式跑相同请求，对照响应 |
| Phase B | 主 | 副 | 流量按 account 灰度切到 Go 版 |
| Phase C | 副 | 主 | Python 版下线，Go 版主 |
| Phase D | — | 唯一 | Python/Rust/C++ 仓库归档 |

#### 11.4.1 灰度切流实现

切流通过 API 网关（Caddy / Envoy / Nginx）按 header 路由，**不依赖代码侵入**：

| 路由键 | 实现 | 例子 |
|-------|------|------|
| 按 account | 网关查 account → backend 表 | `acct_001` → Go 版，其余 → Python 版 |
| 按 header | `X-OpenViking-Canary: go` | 客户端显式指定 |
| 按百分比 | 网关 weighted round-robin | 10% 流量到 Go 版 |
| 按 endpoint | 路径前缀分流 | `/api/v1/mcp/*` → Go 版（MCP 灰度先走 Go） |

**网关配置示例**（Caddyfile）：

```caddy
api.openviking.ai {
    @canary header X-OpenViking-Canary go
    handle @canary {
        reverse-proxy go-backend:8080
    }
    handle /api/v1/mcp/* {
        reverse_proxy go-backend:8080
    }
    handle {
        reverse_proxy py-backend:8000
    }
}
```

#### 11.4.2 双向数据同步

| 数据 | 同步方向 | 机制 |
|-----|---------|-----|
| ragfs 文件 | 双向 | 共享同一 S3 bucket（或 NFS）；元数据文件 schema 兼容，双向可读 |
| 向量库 | 单向（Python → Go） | Python 版 export → ovpack → Go 版 import；Go 版不回写到 Python |
| SQLite（cursor/audit） | 单向（Python → Go） | Go 版只读 Python 版的 cursor；audit 各写各的，对账周期合并 |
| 配置 | 单向（Python → Go） | 同一份 YAML；Go 版只读 |
| Session | 单向（Python → Go） | 通过 `session/export` API 导出 JSON，Go 版 `session/import` 导入 |

**冲突避免**：

- 同一 account 不能同时被两个版本写入（网关路由保证）
- 切流前 Go 版执行 `openviking-migrate sync --from=py --account=acct_001` 全量同步
- 切流回滚后 Python 版执行 `openviking-migrate sync --from=go --account=acct_001`

#### 11.4.3 回滚触发条件与流程

| 触发条件 | 阈值 | 自动/手动 |
|---------|-----|---------|
| Go 版错误率 | > 1%（5xx）持续 5min | 自动切回 |
| Go 版 P99 延迟 | > Python 版 2× 持续 10min | 自动切回 |
| 关键 API 兼容性失败 | golden test 失败 | 自动切回 |
| 数据不一致 | sync 校验失败 | 手动 |
| 客户反馈 | — | 手动 |

**回滚流程**（< 5 分钟完成）：

1. 网关切流：`curl admin:8080/route -d 'acct_001=py'`（or 修改 Caddyfile + reload）
2. Go 版停止接收新请求（graceful shutdown 30s）
3. 数据反向同步：`openviking-migrate sync --from=go --account=acct_001`
4. Python 版恢复服务
5. 事后分析：抓 Go 版日志 + OTel trace + pprof

#### 11.4.4 兼容窗口

| 阶段 | Python 版状态 | Go 版状态 | 时长 |
|-----|--------------|----------|------|
| Phase A-B | 主分支活跃 | 影子/灰度 | 3 个月 |
| Phase C | 维护分支（只接收 bugfix） | 主分支 | 6 个月 |
| Phase D | 归档（tagged LTS） | 唯一 | — |
| LTS 退役 | — | 唯一 | 上线后 12 个月 |

**LTS 承诺**：

- Phase D 后 Python 版打 `v3.x-lts-final` 标签，仅接受安全补丁 12 个月
- 12 个月后归档，README 标注 `unsupported`
- 客户迁移支持窗口：从 Phase D 起至少 6 个月

---

## 12. 风险与对策

### 12.1 技术风险总览

| ID | 风险 | 概率 | 影响 | 对策 |
|----|------|-----|-----|------|
| R1 | API 行为细微不一致 | 中 | 高 | 用现有 SDK test 做 golden test；影子模式对照 |
| R2 | session 压缩算法移植偏差 | 中 | 高 | 逐 case 对照 Python 输出；保留 v2/v3 双实现 |
| R3 | 检索质量回归 | 中 | 高 | ragas 等价评估 + benchmark 对照 |
| R4 | local vectordb 性能下降 | 高 | 中 | 默认推荐 qdrant；local 仅做兜底 |
| R5 | Mooncake/Yuanrong 后端缺失 | 高 | 低 | 走 HTTP 代理；README 标注限制 |
| R6 | 文档解析覆盖率（.doc 等） | 中 | 中 | 降级为文本提取；用户提供样本时再补 |
| R7 | cgo 依赖（unioffice/tree-sitter cgo 变体） | 低 | 中 | 优先选纯 Go 实现；cgo 仅作可选 |
| R8 | go-openai 落后于 OpenAI 新特性 | 中 | 低 | 关键特性自写 HTTP 客户端兜底 |
| R9 | tree-sitter Go binding 覆盖度 | 低 | 中 | 缺语言时回退到正则 |
| R10 | QQ 渠道 SDK 缺失 | 高 | 低 | 自写 HTTP SDK 或不支持的渠道标 deprecated |
| R11 | 安全加固覆盖不全 | 中 | 高 | 详见 [12.9](#129-安全加固)：输入校验/SSRF/注入/脱敏/CORS 全链路 |

### 12.2 local vectordb 性能

**问题**：C++ engine 用 SSE3/AVX2/AVX512 多变体 + LevelDB，纯 Go（hnswlib-go + bbolt）预计性能下降 2-5×。

**对策**：

1. 默认部署走 qdrant 后端（推荐）
2. local 后端作为兜底（小数据量 < 10 万向量）
3. 性能关键场景可选 cgo：`github.com/mnemoe/hnswlib-cgo` 桥接 C++ 实现
4. 提供 `openviking-doctor vectordb-bench` 工具辅助选型

### 12.3 Mooncake / Yuanrong 后端无法直接复用

**问题**：原 Rust 后端依赖 Mooncake（独立 C++ 仓库）与 Yuanrong（FFI），Go 无对应绑定。

**对策**：

1. **不实现** Mooncake/Yuanrong Go 后端
2. README 标注限制，建议走 Redis 或 HTTP 代理
3. 如有强需求，提供 HTTP sidecar 模式：原 Rust crate 编译为小型 HTTP 服务，Go 通过 `internal/ragfs/cache/http.go` 调用

### 12.4 QQ 渠道无成熟 Go SDK

**对策**：

1. 自写 HTTP 客户端 `internal/bot/channels/qq/http.go`
2. 仅支持基础收发消息
3. 文档标注 `experimental`

### 12.5 Sandbox 集成

**问题**：Python 版用 `opensandbox`/`agent-sandbox` 在容器内执行代码。

**对策**：

1. Go 版用 `containerd/containerd` 或 `runp+podman` 调用
2. 也可走外挂 sidecar：保留 Python sandbox 为独立服务，Go 通过 HTTP 调用
3. 默认形态：用 `exec` + `bubblewrap`/`firejail` 做轻量沙箱

### 12.6 业务逻辑迁移遗漏

**问题**：Python 版部分逻辑散落在 `service/` 与 `storage/` 中（如 `viking_fs.py` 160K 行），易遗漏。

**对策**：

1. 先写模块映射表（已在第 6 章给出）
2. 每个模块迁移时，对照 Python 源文件逐函数检查
3. 用 Python 版输出作为 golden test fixture
4. CI 增加 lint：禁止 `internal/` 中存在未覆盖对应 Python 文件的目录

### 12.7 性能回归监控

**对策**：CI 在 main 分支运行 benchmark，`benchstat` 对照基线，回归 > 10% 阻断合并。

### 12.8 第三方 SDK 维护风险

**对策**：

1. 所有外部 SDK 通过 interface 注入（如 `Embedder`、`Reranker`、`VLM`）
2. 关键 SDK（go-openai、eino、qdrant-go、mcp-go、fosite）锁定版本，升级走单独 PR
3. 兜底实现：每个 provider 都有 HTTP 直连的 fallback

### 12.9 安全加固

| 关注点 | 对策 |
|------|------|
| 输入验证 | 所有用户输入通过 `validator/v10` 校验；路径参数做 path traversal 防护（拒绝 `..`、符号链接逃逸） |
| SSRF 防护 | HTTP 代理/爬虫出站请求限制内网 IP 段（`10.0.0.0/8`、`172.16.0.0/12`、`169.254.0.0/16`、`127.0.0.0/8`） |
| 注入防护 | SQL 用 sqlc 参数化查询；shell 执行禁止用户输入拼接；模板渲染转义 |
| 密钥管理 | API key 用 argon2id 哈希存储；secrets 走环境变量或 vault，不入配置文件/日志 |
| 依赖审计 | CI 运行 `govulncheck` + `trivy fs`；定期 `go mod tidy` 清理未用依赖 |
| Header 安全 | 默认添加 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Strict-Transport-Security` |
| 日志脱敏 | slog handler 拦截 `password`/`token`/`secret` 等字段，替换为 `[REDACTED]` |
| CORS | 生产环境限定 `Access-Control-Allow-Origin` 白名单，禁止通配符 `*` |

---

## 附录 A：与现有 design 文档的关系

| 现有文档 | 关系 |
|---------|------|
| `parser-two-layer-refactor-plan.md` | Go 版直接落地两层架构 |
| `session-memory-extraction-flow.md` | Go 版 session 模块遵循同一流程 |
| `local-embedding-llama-cpp-design.md` | Go 版改为 HTTP sidecar 方案 A |
| `mcp-oauth2-1.md` | Go 版用 fosite 实现，行为一致 |
| `memory-link-design.md` | Go 版 resource_memory_link_service 等价实现 |
| `metric-design.md` | Go 版 metrics 直接复用指标命名 |
| `git-version-control-design.md` | Go 版 ragfs/git 用 go-git 实现 |
| `tool-stub-design.md` | Go 版 tool_skill_utils 等价实现 |
| `traj-exp-experience-learning-redesign.md` | Go 版 session/train 等价实现 |
| `openclaw-agent-experience-memory-design.md` | Go 版 ingest/sources/openclaw 等价 |
| `code-tools-design.md` | Go 版 parse/parsers/code 等价 |

## 附录 B：参考资料

- Go 1.22 标准库路由：https://pkg.go.dev/net/http#ServeMux
- Gin：https://github.com/gin-gonic/gin
- otelgin：https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation/github.com/gin-gonic/gin/otelgin
- asynq：https://github.com/hibiken/asynq
- mcp-go：https://github.com/mark3labs/mcp-go
- fosite：https://github.com/ory/fosite
- eino：https://github.com/cloudwego/eino
- go-openai：https://github.com/sashabaranov/go-openai
- qdrant-go：https://github.com/qdrant/go-client
- hnswlib-go：https://github.com/eval-vit/hnswlib-go
- tree-sitter-go：https://github.com/smacker/go-tree-sitter
- bubbletea：https://github.com/charmbracelet/bubbletea
- go-git：https://github.com/go-git/go-git
- colly：https://github.com/gocolly/colly
- otel-go：https://github.com/open-telemetry/opentelemetry-go
- testcontainers-go：https://github.com/testcontainers/testcontainers-go
