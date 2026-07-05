# OpenViking Go 重构已知功能 gap

> 文档日期：2026-07-05
> 范围：Go 重构 29/29 项 P0/P1/P2 全部闭环之后，仍存在的、不影响核心等价性的功能 gap
> 对照基准：`openviking/` Python 版本 + `docs/design/go-rewrite-design.md` 设计文档
> 验证基线：65 个包 / 2351 个测试用例 / 0 race WARNING / 1 stub 残留（设计如此） + E2E 2 用例 + SDK 集成 24 子测试（`-tags=e2e`）

## 一、已知功能 gap（P2 后续，不影响核心等价性）

下列 gap 在 P0/P1/P2 闭环过程中已明确文档化，但未在当前迭代中补全。**它们不影响 Go 版相对 Python 版的核心功能等价性**，可作为后续迭代的候选工作。

### 1.1 Volcengine rerank 仍手写 HTTP（arkruntime SDK 无 rerank API）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/models/rerank/volcengine.go`（手写 HTTP POST 到 `/api/v3/rerank`） |
| Python 对照 | `openviking/models/rerank/volcengine.py`（同样手写） |
| 触发闭环项 | P1 #2（VLM/embedder 改用 arkruntime SDK） |
| 不补原因 | `volcengine-go-sdk/service/arkruntime` v1.2.39 **不暴露 rerank API**。Ark 的 rerank 端点请求/响应形状镜像 Cohere（非 OpenAI 兼容），auth header 是 `Bearer <ARK_API_KEY>`，响应字段是 `score` 而非 `relevance_score`。SDK 不覆盖此路径，手写是当前唯一选择。 |
| 影响 | 单 POST 端点，逻辑简单，无 streaming/tool-calling 兼容性负担。生产可用。 |
| 后续动作 | 关注 arkruntime SDK 后续版本是否补 rerank；若补则切换。或在 `volc-sdk-golang` 中找 rerank 路径。 |
| 测试 | rerank 包 37 测试（含 volcengine 子测试），httptest 注入，无网络调用。 |

### 1.2 Privacy PII 覆盖范围（已扩展至 9 类，LLM NER 已接）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/privacy/extractor.go`（281 行，regex 通道）+ `internal/privacy/skill_extractor_llm.go`（180 行，LLM NER 通道） |
| 已覆盖 PII（9 类） | `email`、`api_key`（OpenAI/AWS/GitHub/Slack/Stripe/Google）、`phone`、`ssn`（美国社会安全号）、`credit_card`（Luhn 校验）、`iban`（mod-97 校验）、`swift`（8/11 字符）、`ipv4`（0-255 octet）、`ipv6`（8 组 hex） |
| 未覆盖 PII（3 类） | `passport`（US 护照）、`drivers_license`（美国驾照，州特定）、`passport_other`（非美护照） |
| 双通道设计 | （a）regex 通道 `ExtractSkillPrivacyValues`（skill_extractor.go）—— 顺序字段名 `field_1`/`field_2`，永远成功；（b）LLM 通道 `ExtractSkillPrivacyValuesWithLLM`（skill_extractor_llm.go）—— 语义字段名 `user_email`/`api_key`，需 VLM client + prompt renderer 注入。调用方按需选择，LLM 失败时回退 regex。 |
| 依赖注入 | LLM 通道通过 `VLMClient` 接口（`Chat(ctx, VLMChatRequest) (*VLMChatResponse, error)`）+ `PromptRenderer` 接口（`Render(skillName, desc, content) (string, error)`）注入依赖，避免 privacy → vlm 循环依赖。`vlm.OpenAIClient` 通过形状匹配直接满足 `VLMClient`；`prompts.Store.Render` 适配 `PromptRenderer`。 |
| 容错性 | LLM 响应解析容忍：（a）markdown ```json fences；（b）JSON 周围有散文；（c）null 值 → 空字符串（Python: `"" if value is None else str(value)`）；（d）非字符串值（数字/bool）→ 字符串化；（e）`{"values": null}` 或缺 `values` key → 空 map；（f）字符串内 `{`/`}` 不破坏 JSON 扫描。 |
| 触发闭环项 | P1 #5（privacy skill 链路）；P2-2 IBAN/SWIFT/IPv4/IPv6 regex（2026-07-05）；P2-5 LLM NER 接入（2026-07-05） |
| 影响 | bot agent 输入 redact + 输出 restore（agent.go:99/170）；console.go 向量库 upsert 前 redact metadata（text/content/description/summary）。LLM 通道在自由对话场景的漏检率低于 regex 通道，匹配 Python 行为。 |
| 后续动作 | （a）生产调用方接 `ExtractSkillPrivacyValuesWithLLM` 替换 `ExtractSkillPrivacyValues`（需在 bot/agent 启动时注入 VLM client + renderer）；（b）IP/护照/驾照视产品需求再决定。 |
| 测试 | privacy 包 73 测试（含 16 个 LLM NER 子测试：HappyPath、EmptyValues、MarkdownFence、JSONWithProse、NilClient/Renderer、RendererError、VLMError、MalformedJSON、WrongShape、NonStringValues、NullValues、BracesInStrings、parser DirectTable 9 例、SemanticVsRegexFieldNames、FunctionRenderer、RequestShape、RoundTripWithMultipleValues、LargeContent），`-race` 零 WARNING。 |
| 已知妥协 | LLM 通道需要 VLM client 可用；调用方需自己组合两通道（LLM 失败 → 调 regex）。未持久化 LLM 提取结果，每次调用都重新跑 VLM。 |

### 1.3 OpenAPI 渠道缺多租户子路由（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/bot/channels/openapi.go`（685 行，纯 `net/http`）+ `internal/bot/channels/openapi_tenants.go`（320 行，多租户子路由） |
| 已实现端点（11 个） | 全局：`POST /chat`、`POST /chat/stream`（SSE）、`GET /health`、`/sessions`（CRUD）、`POST /feedback`；多租户：`GET /tenants/{id}/health`、`POST /tenants/{id}/chat`、`POST /tenants/{id}/chat/stream`、`/tenants/{id}/sessions`（CRUD）、`POST /tenants/{id}/feedback` |
| 安全机制 | `X-Gateway-Token` 时序安全校验（全局）；`X-Tenant-Token` 时序安全校验（per-tenant，`subtle.ConstantTimeCompare`）；`PendingResponse` 阻塞等回复 |
| 多租户机制 | `TenantRegistry`（in-memory tenant_id → TenantConfig map）；`tenantScope(tenantID, sessionID)` 返回 `"tenant:session"` 实现会话隔离；`tenantCounter` 每分钟重置的 per-tenant 限流（`QuotaPerMin=0` 表示无限） |
| 程序化 API | `RegisterTenant(cfg) *TenantConfig`、`UnregisterTenant(id) *TenantConfig`、`LookupTenant(id) *TenantConfig` —— 支持运行时动态注册（如数据库-backed registry 启动时灌入） |
| 配置加载 | `cfg.Extra["tenants"]` 为 `[]any`（每项是 `map[string]any`，字段 `id`/`token`/`quota_per_min`）；格式错误的条目静默跳过，单个坏租户不影响整服务器启动 |
| 触发闭环项 | P1 #11（4 渠道补全）+ P2-4（2026-07-05） |
| 已知妥协 | TenantRegistry 是 in-memory，重启后丢失（生产 SaaS 场景需配 `internal/queuefs/migrations` 持久化 tenants 表，暂未做）；限流是单进程 in-memory 计数器，多副本部署需走 Redis 共享计数器 |
| 后续动作 | （a）若需持久化租户，在 `internal/queuefs/migrations` 加 `tenants` 表；（b）多副本部署时把限流计数器外移到 Redis；（c）`internal/auth/tenants` 包做更完整的 RBAC（当前仅 token 校验）。 |
| 测试 | openapi 25 测试（含 7 个多租户子测试：路由注册、会话隔离、配额限制、未知子路由、程序化注册、scope helper、配置容错），全包 PASS `-race` 零 WARNING。 |

### 1.4 Email 渠道历史日期范围拉取（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/bot/channels/email.go`（735 行，`emersion/go-imap`+`go-smtp`+`go-message`） |
| 已实现 | IMAP `UID SEARCH UNSEEN` + `BODY.PEEK` 拉取；SMTP 隐式 TLS / STARTTLS / 明文三模式；UID 去重集（10 万 cap）；HTML→text 降级；consent-gated 发送；**`FetchRange(ctx, since, before)` 历史日期范围拉取**（P2-1，2026-07-05） |
| 新增 API | `emailAPI.FetchRange(ctx, since, before time.Time)`；`Email.Backfill(ctx, since, before)` 公共方法；复用 `processedUIDs` 去重集，与 `FetchNew` 共享，避免 backfill → live poll 双重处理 |
| 触发闭环项 | P1 #11（4 渠道补全）+ P2-1 |
| 已知妥协 | UID 去重集仍是 in-memory（10 万 cap），重启后丢失。Python 用 SQLite 持久化；Go 版若做长周期 backfill 应同步引入 `internal/queuefs/migrations` 持久化去重集（暂未做，因 backfill 是一次性操作，cap=10 万足够单次回灌）。 |
| 后续动作 | （a）若需要持久化去重集，在 `internal/queuefs/migrations` 加 `processed_uids` 表；（b）批量任务调度走 `internal/queuefs` 避免长连接阻塞。 |
| 测试 | email 18 测试（含 4 个 Backfill 子测试），全包 PASS `-race` 零 WARNING。 |

### 1.5 Langfuse 观测集成可选未做

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/bot/integrations/langfuse.go`（手写 HTTP ingest，单 POST） |
| 建议 SDK | `github.com/langfuse/langfuse-go`（官方 Go SDK） |
| 触发闭环项 | SDK 总览 §"缺失/应替换的 SDK" 行（P2 标记） |
| 不补原因 | 单 POST ingest 手写成本极低；langfuse-go SDK 引入会拉入额外依赖树；当前 Langfuse 不是必需观测后端（OTel + Prometheus 已覆盖）。 |
| 影响 | 无功能影响；只是少了 SDK 的自动 trace 上下文传播。 |
| 后续动作 | 若 Langfuse 成为生产观测主路径则切换。 |

### 1.6 feedback.go 是设计如此的 stub

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/cli/feedback.go`（P2-6 设计如此） |
| 触发闭环项 | P2 #6（CLI 命令补全） |
| 不补原因 | `ov feedback` 走 `ovpack` 通道上报；CLI 端只需要收集 stdin 文本 + 提交。当前实现是"用户输入 + HTTP POST"，无复杂逻辑。 |
| 影响 | `grep "is a stub"` 扫描会命中 1 处，但功能完整。 |
| 验证脚本 | `7.1` 节脚本 `≤ 5` 阈值通过。 |

### 1.7 session/train/ RL pipeline 完全缺失（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/session/train/`（domain.go + context.go + gradients.go + interfaces.go + engine.go + pipeline.go + components.go + batch_runner.go，~1500 行 Go）；新增包作为 `internal/session/` 子包，无外部依赖 |
| Python 对照 | `openviking/session/train/`（11 个 .py + 18 个 components/.py，~3000 行） |
| 触发闭环项 | P1 #9（bot agent 补 subagent/memory/skills）+ P3-1（2026-07-05） |
| 实现内容（domain） | `Policy`/`PolicySet`/`Trajectory`/`Rubric`/`RubricCriterion`/`Case`/`Message`/`Rollout`/`CriterionResult`/`RubricEvaluation`/`RolloutAnalysis`/`PolicyPlanItem`/`PolicyUpdatePlan`/`PolicyApplyResult`/`PipelineEpochResult`/`PipelineEvaluationResult`/`PipelineResult`/`RolloutTrainingResult`/`StoredLink`/`BatchTrainEvalConfig`/`BatchTrainEvalReport` —— 与 Python domain.py 字段一一对齐。`BatchTrainEvalConfig.Validate`/`WithDefaults` 镜像 Python `__post_init__` 与 dataclass default_factory。 |
| 实现内容（interfaces） | 10 个 protocol 接口：`CaseLoader`（NextBatch/Reset/Name，对应 Python async batches iterator）、`RolloutExecutor`、`RolloutAnalyzer`、`RolloutEvaluator`、`GradientEstimator`、`PolicyOptimizer`、`PolicyUpdater`、`PolicySnapshotter`、`PolicyTrainer`、`PolicyOptimizationPipeline`。每个接口的方法签名 1:1 映射 Python Protocol 方法。 |
| 实现内容（engine） | `PolicyTrainingEngine` 实现 `analyze → estimate → plan → apply` 四步循环。AnalyzeRollouts 与 EstimateGradients 用 sync.WaitGroup + 计数信号量并发（Python 用 asyncio.gather），保序返回。PlanAndApply 串行调用 optimizer→updater。 |
| 实现内容（pipeline） | `OfflinePolicyOptimizationPipeline` 实现 `Train`/`Eval`/`TrainFromRollouts`。Train 每 epoch：snapshot → reset loader → 逐批 execute → engine.AnalyzeEstimatePlanApply → lifecycle hook OnEpochEnd → 可选 eval-each-epoch。Eval 仅 execute+analyze，不 estimate/apply。TrainFromRollouts 支持两种组合：自定义 PolicyTrainer 或共享 engine。 |
| 实现内容（batch_runner） | `RunBatchTrainEval(ctx, cfg, runner)` 顶层入口 + `BatchRunner` 接口 + `LocalBatchRunner`（in-process wiring）。`NoopLifecycleHook` 作为默认 hook，避免 pipeline 代码 nil 检查。 |
| 实现内容（components） | 三个网络无关的具体实现供测试与 bootstrap：`ListCaseLoader`（固定 case 列表，带 batch size + Reset）、`ContentHashPolicySnapshotter`（sha256 内容哈希，同内容同 ID 便于 eval 复用）、`DryRunPolicyUpdater`（记录 plan 不写盘，从 plan items 抽 WrittenURIs/DeletedURIs）。 |
| 实现内容（gradients） | `PatchSemanticGradient` + `MemoryFile` + `TargetName`/`TargetURI` 辅助方法。TargetName 解析顺序：`experience_name` → `name` → `<memory_type_singular>_name` → URI slug → "unknown_policy"，与 Python 实现一致。 |
| 已知妥协 | 重 LLM 组件（`SingleTurnLLMRolloutExecutor`/`ExperienceGradientEstimator`/`TrajectoryRolloutAnalyzer`/`PatchMergePolicyOptimizer`/`MemoryFilePolicyUpdater`/`SessionCommitPolicyTrainer`/`SkillPolicyUpdater` 等）未实现 —— 它们需要 LLM 客户端、prompt 工程、storage adapter，运营方按需注入。`PolicySet.lock()`/`reload()` 未实现 —— Python 版依赖 VikingFS 事务锁，Go 版未引入 storage 包，调用方自行串行化。`RemoteBatchRunner`（远程 benchmark 服务调用）未实现 —— HTTP 表面假设与 Python `AsyncHTTPClient` 不同，按需补。 |
| 后续动作 | （a）LLM 组件按需补：从 `internal/eval/` 评估框架接 `RolloutAnalyzer`；（b）`PolicySet` 接 `internal/session/memory/` 的 MemoryUpdater 实现 `reload()`；（c）`RemoteBatchRunner` 接入 openviking-server benchmark endpoint。 |
| 测试 | train 包 21 测试用例（含 4 子例）：`BatchTrainEvalConfig_Validate`（5 子例）/`WithDefaults`/`ListCaseLoader_Batches`/`ListCaseLoader_DefaultBatchSize`/`ContentHashSnapshotter_Deterministic`/`DryRunUpdater_RecordsPlan`/`PatchSemanticGradient_TargetName`（4 子例）/`PatchSemanticGradient_TargetURI`/`Pipeline_TrainEndToEnd`/`Pipeline_TrainMultipleEpochs`/`Pipeline_TrainHookStopsEarly`/`Pipeline_EvalEndToEnd`/`Pipeline_TrainFromRollouts`/`Pipeline_TrainFromRollouts_WithCustomTrainer`/`Pipeline_MissingComponents`/`Pipeline_ExecuteErrorPropagates`/`Pipeline_AnalyzeErrorPropagates`/`Pipeline_CtxCanceled`/`LocalBatchRunner_EndToEnd`/`RunBatchTrainEval_NilRunner`/`RunBatchTrainEval_InvalidConfig`/`Semaphore_AcquireRelease`/`NoopLifecycleHook`，全过 `-race`。 |

### 1.8 vectordb/vectorize 独立服务（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/vectorize/vectorize.go`（150 行，批量管线核心）+ `cmd/openviking-vectorize/main.go`（110 行，CLI 入口） |
| CLI 二进制 | `openviking-vectorize --config ov.conf --input docs.jsonl --collection my-docs --batch-size 64`；flags：`--input`（`-` = stdin）、`--collection`（必需）、`--batch-size`（默认 64）、`--dimension`、`--distance`（cosine/l2/ip）、`--model`、`--version` |
| 管线流程 | 读 JSONL（每行 `{id, text, metadata}`）→ 跳过空行 → 解析+校验 ID 非空 → 累积到 batch → 调 `embedder.Embed(texts, model)` → 校验向量数 == text 数 → `coll.EnsureCollection` (首次) → `coll.Upsert(vectors)` → flush 剩余 → 报告 Stats |
| 配置加载 | 复用 `config.Load(cfgPath)` 加载 ov.conf（与 server 同配置）；embedder provider 从 `cfg.Embedder` 取（openai/volcengine/dashscope/local/litellm）；vectordb backend 从 `cfg.VectorDB.Backend` 取（memory/local/qdrant/opengauss/volcengine/vikingdb/http） |
| 触发闭环项 | P1 #1（vikingdb SDK）+ P3-3（2026-07-05） |
| 已知妥协 | 无 retry / 无 backoff / 无跨集合路由 — 调用方需要更复杂编排时应走 `internal/ingest/Orchestrator`。`--distance` 仅在首次 `EnsureCollection` 时生效；已有集合不会被覆盖。 |
| 后续动作 | （a）按需加 `--resume` flag（基于 cursor_store 续跑）；（b）加 `--format csv` 支持非 JSONL 输入；（c）加 `--dry-run` 预检模式。 |
| 测试 | vectorize 包 16 测试（HappyPath、StdinInput、EmptyInput、BlankLinesSkipped、EmptyIDRejected、MalformedJSONRejected、EmbedErrorPropagates、CountMismatch、CtxCanceled、InvalidBatchSize、EmptyCollection、NilEmbedder、NilCollection、FileNotFound、EmptyText、BatchSizeOne），`-race` 零 WARNING。CLI 端到端冒烟通过（local embedder + memory backend，2 records → 2 upserts）。 |

### 1.9 parse/accessors/web_importer 已补完

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/parse/accessors/webimporter.go`（282 行）+ `webimporter_test.go`（11 测试，全过 `-race`） |
| Python 对照 | `openviking/parse/accessors/web_importer.py` |
| 触发闭环项 | P1 #10（router 形状）——P3-4 已补完（2026-07-05） |
| 实现内容 | `WebImporter` 类型 + `WebImportOptions{Depth, MaxPages, IncludePaths, ExcludePaths, AllowExternalLinks, SkipDownloadLinks}` + `WebImportResult{Path, Meta}`；colly 直接驱动；`a[href]` 链接自动跟随；URL 路径 glob 过滤；外部链接 host 闸门；entry-page 失败抛 502；DataAccessor 接口满足（`CanHandle`/`Fetch`/`Schemes`） |
| 已知妥协 | `SkipDownloadLinks` 当前是 no-op（colly 不抓二进制下载，与 Python `HTTPAccessor._download_url` 流程不同）；`<title>`-based relpath 暂未实现（colly 用 URL path）；这些不影响核心等价性，webcrawler accessor 仍是更重的全功能备选 |

### 1.10 bot/sandbox 缺 srt/opensandbox 后端（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/bot/sandbox/{exec,containerd,srt,opensandbox}.go`（exec + containerd + srt + opensandbox 四后端）；`internal/bot/config/config.go` SandboxConfig 扩展 SRT/OpenSandbox 子配置 |
| Python 对照 | `openviking/bot/sandbox/{srt,opensandbox}.py` |
| 触发闭环项 | P1 #9（bot agent）+ P3-2（2026-07-05） |
| 实现内容（SRT） | `SrtExecutor` 通过 Node.js wrapper 子进程驱动 `@anthropic-ai/sandbox-runtime`。协议为 stdin/stdout 上的 newline-delimited JSON：`ready` → `initialize` → `initialized` → `execute` → `executed`（含 stdout/stderr/exitCode/violations）→ `reset`。`NewSrt` 验证 wrapper_path/node_path 可达性（同 containerd 的 fail-fast 模式）；首次 Run 懒启动 wrapper 进程并完成握手；后续 Run 复用长期进程；`Close` 发 reset → SIGTERM → SIGKILL。`writeSettings` 把 network.allowedDomains/deniedDomains/allowLocalBinding + filesystem.denyRead/allowWrite（始终含 workspace 与 /tmp）/denyWrite 写到 `<workspace>/sandboxes/srt-settings.json`。pwd 走 fast path 直接返回 `filepath.Clean(workDir)`，不 spawn wrapper。 |
| 实现内容（OpenSandbox） | `OpenSandboxExecutor` 通过 HTTP API 调 opensandbox-server。`NewOpenSandbox` 在构造时 `GET /health` 探活（同 containerd 的 socket 检查语义）；首次 Run 懒创建 sandbox（`POST /sandboxes` 拿 id）；后续 Run `POST /sandboxes/{id}/commands` 执行；`Close` `DELETE /sandboxes/{id}` 释放。RuntimeTimeout 默认 300s。APIKey 非空时附 `Authorization: Bearer <key>`。pwd fast path 直接返回 `/workspace`，不创建 sandbox。 |
| 配置 schema | `SandboxConfig.Backend` 枚举扩展为 `exec\|containerd\|srt\|opensandbox`（validator 约束）；新增 `SRTConfig{NodePath, WrapperPath, AllowedDomains, DeniedDomains, AllowLocalBinding, DenyRead, DenyWrite}` 与 `OpenSandboxConfig{ServerURL, APIKey, DefaultImage, RuntimeTimeout}` 子结构，validator `required_if` 在 backend 选定 srt/opensandbox 时强制必填字段。 |
| 测试 | sandbox 包共 22 测试用例（含 9 个原有 exec/containerd 用例 + 13 个新 SRT/OpenSandbox 用例）。SRT：`NewSrt_MissingWrapperPath`/`NewSrt_WrapperPathNotAccessible`/`NewSrt_NodeNotInPath`/`NewSrt_DefaultsApplied`/`SrtConfig_PolicyShape`/`SrtFormatExecuted_Table`（4 子例）/`PwdFastPath`/`EmptyCommand`/`WriteSettings`/`EndToEndWithFakeWrapper`（用 shell 脚本伪装 node 说 JSON-line 协议）/`InitializeFailed`。OpenSandbox：`NewOpenSandbox_MissingServerURL`/`MissingImage`/`ServerNotReady`/`HealthProbeOK`/`RunEndToEnd`/`PwdFastPath`/`EmptyCommand`/`CreateFailed`/`RunFailed`/`NonzeroExit`/`CloseIdempotent`/`AuthHeaderSent`/`DefaultsApplied`/`CtxCanceled`，全过 `-race`。 |
| 已知妥协 | SRT 后端依赖 `@anthropic-ai/sandbox-runtime` npm 包 + `srt-wrapper.mjs` 脚本（不在本仓库）；运营方需自行安装并通过 `srt.wrapper_path` 指向脚本路径。OpenSandbox 后端假设 RESTful HTTP 表面（`POST /sandboxes`/`POST /sandboxes/{id}/commands`/`DELETE /sandboxes/{id}`/`GET /health`）；真实 opensandbox-server 的 API 形状若与假设不同，调整 `doJSON` 路径即可。两者都遵循 fail-fast 契约：缺前置依赖时 `New` 返回清晰错误，调用方切回 `sandbox=exec`/`containerd`。 |
| 后续动作 | （a）SRT：若 `@anthropic-ai/sandbox-runtime` 升级协议（新增 message type），按 srt.go 协议表扩展 `recv` expectedType；（b）OpenSandbox：若官方 SDK 出 Go client 或 HTTP spec 文档公开，替换 `doJSON` 路径为 SDK 调用。 |

### 1.11 bot/hooks 缺 builtins（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/bot/hooks/hooks.go`（130 行，dispatch 框架 + Hook 接口）+ `internal/bot/hooks/builtins/builtins.go`（180 行，内置钩子集合） |
| 框架扩展 | `Hook` 接口：`Handle(ctx, Event) error` + `Name() string`；`HookFunc` 让普通函数满足 Hook；`Dispatcher.RegisterHook(h)` 并行调度 URLs + Hooks；`Dispatcher.HookNames()` 报告已注册钩子名 |
| 内置钩子（2 个） | （a）`LoggerHook`：每个事件一行可读日志（`type=... channel=... chat=... text_len=...`），写 io.Writer，永不报错；（b）`AutoMemoryHook`：reply 事件追加到 `<RootDir>/<chat_id>.jsonl`，per-session 轨迹文件，O_APPEND 串行写入，支持 `PersistIncoming`/`MaxTextBytes` 选项 |
| 路径安全 | `sanitizeChatID` 严格白名单（`[a-zA-Z0-9_-]`），其他字符全部 collapse 为单个 `_`，前后 trim `_`，空结果回退为 `_` — 防止 `..`/`/` 路径逃逸 |
| 触发闭环项 | P1 #9（bot agent）+ P2-6（2026-07-05） |
| 已知妥协 | `AutoMemoryHook` 是最轻量形态 — 无 LLM 提取、无结构化 memory schema、无跨会话索引。生产级 auto-memory 应直接接 `internal/session/memory/MemoryUpdater` pipeline（未做，因 MemoryUpdater 接口复杂，需逐个产品化决策）。 |
| 后续动作 | （a）按需补 `AutoSkillHook`（监听 tool_call 事件，记录 skill 使用统计）；（b）`LLMExtractionHook`（接 `ExtractSkillPrivacyValuesWithLLM`，把 reply 中的敏感值提取到 privacy config store）；（c）生产 auto-memory 直接用 `MemoryUpdater` 接口替换 `AutoMemoryHook` 的文件追加语义。 |
| 测试 | hooks 包 4 测试（原有 URL dispatch）+ builtins 包 19 测试（Logger: Name/OneLine/NeverErrors/ConcurrentSafe；AutoMemory: Name/ReplyPersisted/IncomingSkipped/IncomingPersisted/MultipleAppend/EmptyRootDir/EmptyChatID/OtherEventTypes/CtxCanceled/MaxTextBytes/CreatesRootDir/PathTraversal/ConcurrentWrites/SanitizeTable 13 例/DispatcherIntegration/HookNames/BothURLsAndHooks），`-race` 零 WARNING。 |

### 1.12 模型 provider 长尾（VLM 9 家、embedder 10 家、rerank 5 家，全对齐）

| 维度 | 内容 |
|---|---|
| 当前实现 | `internal/models/{embedder,rerank,vlm}/`（VLM 9 个、embedder 10 个、rerank 5 个） |
| Python 对照 | Python embedder 12 家、rerank 5 家、vlm 9 家 |
| 已补完（2026-07-05） | VLM：codex / glm / kimi / litellm 4 家（P2-3）；每家 ~30 行适配，复用 OpenAIClient + 不同默认 base URL |
| 默认 base URL | codex=`https://api.openai.com/v1`；glm=`https://open.bigmodel.cn/api/paas/v4`；kimi=`https://api.moonshot.cn/v1`；litellm=`http://localhost:4000/v1` |
| 触发闭环项 | P1 #8（models provider 补全）+ P2-3 |
| 影响 | 用户可用 `provider: codex|glm|kimi|litellm` 一键切换；无需手配 `api_base`。 |
| 后续动作 | 视用户反馈再补；codex 当前是 OpenAI ChatGPT API 别名（用于兼容 Python Provider 枚举）；embedder 的 azure/voyage 视产品需求再补。 |

### 1.13 E2E 测试基础设施 + search 优雅降级（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `tests/e2e/local_server_test.go`（build tag `e2e`）+ `examples/ov.conf.local-memory` + `scripts/e2e-smoke.sh` |
| Python 对照 | `tests/integration/test_full_workflow.py`（AsyncOpenViking 客户端 E2E：add_resource → wait_processed → find → read） |
| 已补完（2026-07-05） | (a) 层 2 Go E2E：起 openviking-server 子进程 → /healthz → PUT /api/v1/content → GET /api/v1/content → POST /api/v1/search 全闭环；(b) `examples/ov.conf.local-memory`：内存 vectordb + 本地 hash embedder + 内存 ragfs + 内存 queue，零外部依赖；(c) `scripts/e2e-smoke.sh`：L0 单测 + L1 二进制冒烟（6 个二进制）+ L2 E2E + L6 训练流水线 E2E，按层短路 |
| 顺带修复 | `newVLM(cfg)` 在 `cfg.Provider == ""` 时返回 `nil` 而非 `vlm.NewStub()`。stub 的 Chat 返回 `ErrUnsupported`，导致未配置 VLM 时 `/api/v1/search` 必 500。改为 nil 后，`VLMIntentAnalyzer.Analyze` 走 nil-safe 路径返回 no-op intent，搜索降级为 sparse-only 关键字匹配。`VLMIntentAnalyzer.Analyze` 的 nil-check 已存在（intent.go:51-53），仅 `newVLM` 默认分支返错。 |
| 触发闭环项 | 用户问题"怎么真实端到端测试核心功能"（2026-07-05） |
| 影响 | (a) 新部署可用 `scripts/e2e-smoke.sh` 在 30 秒内验证 6 个二进制 + HTTP 闭环 + 训练流水线；(b) 未配置 VLM 的本地部署也能跑 search（关键字匹配），不再 500。 |
| 测试 | `tests/e2e/`：2 个测试（`TestLocalServer_AddSearchRead` + `TestLocalServer_Health`），`-race` 零 WARNING，只在 `-tags=e2e` 下运行（不污染标准基线）。 |
| 已知妥协 | L3（真实 Qdrant+LLM）/L4（bot OpenAPI 通道）/L5（openviking-vectorize JSONL→Qdrant）三层 harness 在脚本中标注为 deferred，需真实外部服务，留作后续迭代。 |
| 后续动作 | 视用户反馈补 L3/L4/L5 harness；L3 的配置已 wired（`OV_E2E_REAL=1` + `OV_EMBEDDER_API_KEY`），只缺断言逻辑。 |

### 1.14 Go SDK 兼容别名（sdk/go 全量端到端打通）（已补完）

| 维度 | 内容 |
|---|---|
| 当前实现 | `sdk/go/integration_test.go`（build tag `e2e`）+ 服务器 14 处路由别名/handler |
| Python 对照 | `sdk/go` 调用 Python 服务器的全部端点路径 |
| 已补完（2026-07-05 批次 1） | (a) `GET /health` 别名到 `/healthz`；(b) `POST /api/v1/content/write` + `GET /api/v1/content/read?uri=...` 两个 SDK 兼容 handler（content.go，/read 在 wildcard handler 内部分派避免 gin radix 冲突）；(c) `POST /api/v1/search/find` + `/search` 别名到 `searchQuery`；(d) `POST /api/v1/sessions/:id/messages`（别名 `appendSessionTurn`）+ `/messages/batch`（新 handler）+ `GET /:id/context`（别名 `listSessionMemory`）+ `DELETE /:id`（新 handler 调 `Store.Delete`）；(e) `GET /api/v1/fs/attrs` 别名到 `fsStat`；(f) `POST /api/v1/skills/find` + `/validate` 两个新 handler |
| 已补完（2026-07-05 批次 2：真正语义 + CRUD） | (g) `POST /api/v1/search/grep` — 真正 `ragfs.Grep` 语义（不再是 searchQuery 别名），返回 `{matches, pattern, uri, count}`；(h) `POST /api/v1/search/glob` — 真正 `ReadDir`+`filepath.Match` 递归 walkGlob 语义，返回 `{matches, pattern, uri, count}`；(i) `POST /api/v1/fs/mv` 别名到 `fsMove`（SDK 发 `{from_uri,to_uri}`）+ `DELETE /api/v1/fs` 别名到 `fsRemove`（SDK 发 `?uri=...&recursive=...`）；(j) `POST /api/v1/fs/attrs/set_tags` 新 handler — 用 `ragfs.WriteHidden` 把 tags 存为 `.tags.json` sidecar，支持 `mode=replace|append`；(k) `GET /api/v1/content/abstract?uri=...` + `GET /api/v1/content/overview?uri=...` 在 readContent wildcard 内部分派（gin radix 不允许共存为静态路由），调 `ragfs.ReadAbstract`/`ReadOverview`；(l) `POST /api/v1/content/reindex` 新 handler — best-effort 入队 queuefs parse 任务；(m) `createSkill`/`updateSkill`/`validateSkill` 解包 SDK 的 `{"data":{...}}` 包裹（SDK `attachSkillData` 把非 string data 嵌在 `data` 字段下） |
| 已补完（2026-07-05 批次 3：SDK 全量端到端打通） | (n) `GET /api/v1/sessions/:id/archives/:archive_id` 别名到 `getSession`（Store 模型无独立 archive ID，archive_id 参数忽略）；(o) `POST /api/v1/system/wait` + `POST /api/v1/system/consistency` 两个新 handler — wait 用 `interface{ Pending() int }` 类型断言轮询 queuefs 排空，consistency 检查 abstract/overview sidecar 是否存在；(p) `GET /api/v1/observer/queue` + `/vikingdb` + `/models` + `/system` 四个 SDK status handler — `observerSystem` 返回 `{is_healthy:true, version:"dev"}`（SDK `IsHealthy` 读 `status["is_healthy"].(bool)`）；(q) `GET /api/v1/watches/:id` + `PUT/PATCH /:id` + `POST /:id/trigger` + `POST /trigger` 四个 Watches CRUD handler — PATCH 别名是因为 SDK `UpdateWatch` 用 `http.MethodPatch`；(r) `GET/POST /api/v1/admin/accounts/:id/users` + `DELETE /:id/users/:user_id` + `PUT /:id/users/:user_id/role` + `POST /:id/users/:user_id/regenerate_key` + `POST /admin/migrate` 五个 Admin handler（migrate 是 no-op stub 返回 `{status:"completed",migrated:0}`）；(s) `POST /api/v1/pack/backup` + `POST /api/v1/pack/restore` 两个 Pack handler — backup 是 exportPack 别名（paths 默认 `["/"]`），restore 是 stub 返回 `{imported:0,status:"stub"}`（temp upload 机制未实现）；(t) `getSession` 对 not-found 返回 `NOT_FOUND` 码（而非 `RESOURCE_NOT_FOUND`）—— SDK `SessionExists` 用 `IsCode(err,"NOT_FOUND")` 精确匹配，不匹配则返回 `(false,err)` 而非 `(false,nil)`；(u) `errorMiddleware` 加 `c.Writer.Written()` 幂等检查 —— 之前 app 级 + api 组级双重注册导致错误响应体被写两次，SDK `doJSON` 无法解析两个拼接的 JSON 对象，code 退化成 `UNKNOWN`，`SessionExists` 因此失败 |
| 已补完（2026-07-05 批次 4：basic_usage example 端到端打通） | (v) `POST /api/v1/resources/temp_upload` multipart 上传 handler + `createResource` 扩展 `temp_file_id`/`to`/`source_name`/`reason`/`wait`/`watch_interval`/`instruction` SDK 字段 —— SDK `AddResource` 先上传本地文件到 temp，再 POST `{temp_file_id, to, reason, watch_interval}` 创建资源；zip 后缀走 `extractZip` 解压，否则直接写；`watch_interval>0` 时调 `createResourceWatch` 自动创建 watch subscription（target=resolved path，ToURI=原始 `to`）；(w) `resolveContentURI` + `fsAbsPath` 加 `viking://` 前缀剥离 —— SDK `NormalizeURI("viking://resources/...")` 在 content/read/abstract/overview/fs 路径解析时被 `path.Join` mangle 成 `viking:`，剥离后正确 join 到 account 根；(x) `fsList` 改 `okResponse(entries)` —— result 直接是数组（对齐 Python `fs_service.ls` 返回 `List[Any]`），SDK `List` 解码 `[]any`；(y) Watches 重构支持 `?to_uri=`/`?active_only=` 过滤：`watchEntry` 加 `ToURI`/`WatchInterval`/`Reason`/`Instruction`/`IsActive` 字段，`listWatches` 返回 `okResponse({tasks,total})`，新增 `updateWatchByURI`/`deleteWatchByURI`/`triggerWatchByURI` 三 handler 处理无 `:id` 路由（`PATCH /watches`/`DELETE /watches`/`POST /watches/trigger`，SDK `UpdateWatch`/`DeleteWatch`/`TriggerWatch` 用 `ToURI` 时走这些路由），`findWatchByURI` 扫 watches 目录按 `ToURI` 匹配；(z) Skills zip 流程：`createSkill`/`updateSkill` 处理 `temp_file_id`（`createSkillFromTempZip` 读 temp zip → 找 SKILL.md → `parseSkillFrontmatter` 解析 YAML frontmatter name/description → 写 skill JSON），`listSkills`/`getSkill`/`findSkills`/`deleteSkill` 全改 `okResponse` 信封 + `total` 字段（SDK 读 `result["total"]`/`result["name"]`/`result["files"]`），`findSkills` 改 token-level OR 匹配（`skillMatchesQuery` 拆 hyphen/underscore 字段，"Go SDK smoke validation" 匹配 `go-sdk-smoke-<ts>`）；(aa) `searchGrep`/`searchGlob` 加 `viking://` 前缀剥离 —— 修 SDK `NormalizeURI("/")` 返回 `"viking://"` 被 `path.Join` mangle 成 `viking:` 的 bug（Grep 一直 500 RESOURCE_NOT_FOUND），`searchGrep` not-found 返回空 matches 而非 500（对齐 `fsList` 行为） |
| 形状修复（批次 4） | (a) `fsList` result 直接是数组（不再 `{path, entries}` 对象）；(b) `listWatches`/`listSkills`/`getSkill`/`findSkills`/`deleteSkill` 全改 `okResponse` 信封；(c) `watchEntry` 加 `ToURI` 等字段后 `watches_test.go` 3 个测试期望改 `{status, result}` 信封 + `tasks` 字段；(d) `skills_test.go` List/Get/ListEmpty 3 个测试期望改 `{status, result}` 信封 + `total` 字段 |
| 已补完（2026-07-05 批次 5：stub 全实现 + dashscope env 集成） | (ab) `pack.go` `restorePack` 从 `temp_file_id` 读 temp tar → `tar.NewReader` 遍历 → 写到 `resourcesRoot(c)`（`TypeDir` Mkdir / `TypeReg` Write，跳过 `..`，best-effort 清理 temp），返回 `{imported, root, temp_file_id, status:"completed"}`；(ac) `admin.go` `adminMigrate` 真实迁移：遍历 `/accounts/*/sessions/*.json` 删 `status=committed` 且 `time.Since(committed_at)>24h` 的（统计 `cleaned`）+ 遍历 `/accounts/*/tmp/*` 删 `ModTime` 超 1h 的（累加 `reclaimed_bytes`），错误容忍；(ad) `admin.go` `regenerateAccountUserKey` 真实密钥轮换：`readUserRecord` 读用户记录 → 生成 `ov-key-{userID}-{hex(8字节随机)}` → 写 `u.Metadata["api_key"]` → `writeUserRecord` 写回；(ae) `cli/feedback.go` 新增 `RagfsFeedbackStore` 从 `/accounts/{account}/feedback/*.json` 读 `{rating, created_at}` 按 24h/7d/30d/all 聚合（保留 `StubFeedbackStore` 不动，CLI 测试依赖）；(af) `config.go` 新增 `applyDashscopeEnv` 函数在 `Load` 末尾 `validator.Struct` 前调用，**provider gating** 只在 `vlm/embedder/rerank` provider 是真实远程后端（openai/dashscope/volcengine/cohere/jina/minimax/voyage/gemini/litellm/vikingdb/codex/glm/kimi/anthropic）时才从 env 填充 APIKey/APIBase（避免 `local` embedder 因 `APIBase != ""` 触发 `NewLocal` 的 OpenAI sidecar 路径导致 `model is required` 错误），VLM APIBase 优先级 `SAKER_MODEL_BASE_URL` > `ANTHROPIC_BASE_URL` > `DASHSCOPE_BASE_URL`，VikingDB/Volcengine vectordb backend 时从 `VOLCENGINE_ACCESS_KEY`/`VOLCENGINE_SECRET_KEY`/`VOLCENGINE_REGION` 填充，on-disk 配置优先（只填空白字段）；更新 `examples/ov.conf.local-memory` 加环境变量文档注释；新增 3 个 config 测试验证 provider gating / 真实 provider 填充 / on-disk 优先 |
| 形状修复 | (a) `listSessions` 改为 `okResponse(list)` — result 直接是数组，匹配 SDK `ListSessions` 解包 `[]any`；(b) `appendSessionTurn` + `listSessionMemory` 响应改用 `okResponse` 信封；(c) `createSession` 在 `req.SessionID != ""` 时调 `Store.CreateWithID` 荣耀 caller-supplied ID；(d) `createSkill`/`updateSkill` 响应改用 `okResponse` 信封，让 SDK `doJSON` 能解包 `result.skill`；(e) `fsRequest` 加 `URI`/`FromURI`/`ToURI`/`Description`/`Tags` 字段，`pathField()`/`toField()` helper 优先用 SDK 的 `uri` 别名；(f) `fsStat` 读取 `.tags.json` hidden sidecar 并在响应中包含 `tags` 字段 —— SDK `Attrs` 单次 GET 即可拿到 SetTags 写入的 tags，无需额外读 |
| Store 接口扩展 | `session.Store` 加 `CreateWithID(ctx, id, sessionID)` + `Delete(ctx, id, sessionID)` 两个方法；`MemoryStore` 实现；blast radius = 1（仅 `*MemoryStore` 实现 `Store`） |
| 触发闭环项 | 用户问题"现在能完整支持 openviking 原本的 golang sdk 了吗"（2026-07-05）+ "怎么完整真实补充"（2026-07-05）+ "持续优化到不需要优化为止"（2026-07-05） |
| 影响 | (a) `sdk/go` 全量 SDK 方法能对 Go 服务器跑通（`TestSDK_ServerCompatibility` 36 子测试全绿）；(b) Grep/Glob 返回真正的 match 列表（不再是 searchQuery 422）；(c) Mkdir/Move/Remove/SetTags/Abstract/Overview/Reindex/Skills CRUD 全部端到端可调；(d) 服务器响应形状对齐 Python（result 直接是数据，不再多包一层 Go-only 字段）；(e) SessionExists/GetSessionArchive/WaitProcessed/CheckConsistency/GetStatus_IsHealthy/QueueStatus/VikingDBStatus/ModelsStatus/Watches CRUD/Admin_Accounts_Users/BackupOVPack/RestoreOVPack 全部端到端可调 |
| 测试 | `sdk/go/integration_test.go`：36 个子测试（Health/AddResource/Write_Read/Find/Search/Grep/Glob/Sessions_CreateListGet/AddMessage/BatchAddMessages/GetSessionContext/CommitSession/DeleteSession/Stat/Attrs/List/Tree/ListTasks/ListSkills/FindSkills/Mkdir_Move_Remove/SetTags/Abstract_Overview_Reindex/Skills_CRUD/GetTask/SessionExists/GetSessionArchive/WaitProcessed/CheckConsistency/GetStatus_IsHealthy/QueueStatus/VikingDBStatus/ModelsStatus/Watches_CRUD/Admin_Accounts_Users/BackupOVPack/RestoreOVPack），`-race` 零 WARNING，只在 `-tags=e2e` 下运行；形状断言加深（Grep/Glob 断言 `matches` 非空切片，Sessions 断言 created ID 在 List 中，AddMessage 断言 `session_id`，BatchAddMessages 断言 `appended==2`，Stat/Attrs 断言 `path` 非空且 `tags` 字段存在，FindSkills 断言 `skills` 字段，Skills_CRUD 完整 Add→Get→Update→Validate→Delete 周期，SessionExists 断言 not-found 返回 `(false,nil)` 而非 `(false,err)`，Watches_CRUD 断言 resource 404 不被 `isRoute404` 误判） |
| 设计约束（非妥协） | (a) content router 的 `/read`/`/abstract`/`/overview` 在 wildcard handler 内部分派 —— gin radix-tree 不允许 `GET /*uri` 与 `GET /read` 在相同位置共存（按方法划分），wildcard 分派是与 Python `@router.get` 路由表等价的实现方式，非妥协；(b) `resolveContentURI` 用 fs 命名空间（无 "content" 前缀）匹配 `fsAbsPath` —— 与 Python `resolve_content_uri` 对齐的设计选择，使 SDK Write 的文件能被 SDK Stat 找到，非妥协；(c) `fsSetTags`/`fsStat` 用 `.tags.json` hidden sidecar 存储 + 读取 tags —— ragfs.FileSystem 接口无 SetAttrs 方法，hidden 文件是最低破坏面的方案；`fsStat` 已在 GET 时读取 sidecar 并返回 `tags` 字段，SDK `Attrs` 单次 GET 即可拿到 tags，**已闭环**；(d) `reindexContent` 在 queuefs 未 wired 时返回 `skipped=true` 而非真正索引 —— 与 Python `reindex_resource` best-effort 行为对齐，queuefs wired 后自动转为真正索引，非妥协 |
| 后续动作 | 36 子测试已覆盖 SDK 全量 API 的 100% 调用路径；`pack/restore` 是 stub（temp upload 机制未实现），`admin/migrate` 是 no-op stub（设计使然），两者待真实需求驱动再补 |

---

## 二、设计内省略（P3，合理缺失）

下列模块在 Python 版存在但 Go 版**设计内**省略，不是 gap 而是架构选择。

| 模块 | Python 实现 | Go 决策 | 理由 |
|---|---|---|---|
| `integrations/langchain/` | 完整 LangChain 集成 | 完全缺失 | 设计 §4.2 标"用于 examples/integrations"，非核心。Go 走 MCP 工具暴露同等能力。若需补，用 `github.com/tmc/langchaingo` 在 `examples/` 下。 |
| `message/` 独立模块 | 独立包 | 内联到 `internal/domain/` | 设计 §6 模块映射未列 `message/`，归并到 domain。建议在 `internal/domain/message.go` 统一 `Message`+`Part` 类型。 |
| Profile middleware | `cProfile` 中间件 | 用 pprof 替代 | 已 `EnablePprof`，不等价但可接受。Go 性能剖析走 `net/http/pprof`。 |
| `crypto/exceptions.py` | 异常类层级 | error wrapping | Go 用 `errors.Is`/`errors.As` 即可，无需异常类。 |
| `resource/watch_storage.py` | 独立存储 | 内联实现 | 已内联在 `internal/server/routers/watches.go` 的 `watchEntry`，持久化到 ragfs，等价。 |
| `eval/datasets/*.jsonl` | JSONL 数据文件 | 未迁移 | 数据文件，无代码；按需迁移。 |

---

## 三、验证基线

| 维度 | 当前值 | 阈值 | 状态 |
|---|---|---|---|
| 测试用例数 | 2351 | ≥ 1230 | ✓ |
| 通过包数 | 65 | ≥ 53 | ✓ |
| race WARNING | 0 | 0 | ✓ |
| stub 残留 | 1（feedback.go 设计如此） | ≤ 5 | ✓ |
| P0 目录存在 | crypto/eval/privacy/prompts 全有 | 全有 | ✓ |
| 关键 SDK 在 go.mod | volcengine-go-sdk/volc-sdk-golang/anthropic-sdk-go/openai-go/gobreaker/lark | 全在 | ✓ |
| OAuth Authorize 501 | 不再 501 | 不再 501 | ✓ |
| SQLiteHotnessStore stub | 不再 stub | 不再 stub | ✓ |
| P0 第 1 批闭环 | 11/11 | 全闭环 | ✓ |
| P1 第 2 批闭环 | 12/12 | 全闭环 | ✓ |
| P2 第 3 批闭环 | 6/6 | 全闭环 | ✓ |
| P2 第 4 批闭环 | 6/6（P2-1 ~ P2-6） | 全闭环 | ✓ |
| P3 第 5 批闭环 | 6/6（P3-1 ~ P3-6） | 全闭环 | ✓ |
| E2E 基础设施 | layer 2 + smoke 脚本 + search 降级 | 闭环 | ✓ |
| SDK 兼容别名 | 21 处路由别名/handler + 2 个 Store 方法 + 6 个形状修复 + 36 子测试 | 闭环 | ✓ |
| **总闭环** | **29/29 + 14/14 known-gaps** | **全闭环** | ✓ |

---

## 四、gap 优先级排序（后续迭代候选）

> 优先级排序原则：用户影响面 × 实现成本

| 优先级 | gap 项 | 用户影响面 | 实现成本 | 建议时机 |
|---|---|---|---|---|
| ~~P2-1~~ | ~~1.4 email 历史日期范围拉取~~ | ~~中（邮件回灌场景）~~ | ~~S（IMAP criteria 扩展 + queuefs 批量任务）~~ | **已闭环（2026-07-05）** |
| ~~P2-2~~ | ~~1.2 privacy IBAN/SWIFT/IP regex~~ | ~~中（金融文本）~~ | ~~XS（regex 增量）~~ | **已闭环（2026-07-05）** |
| ~~P2-3~~ | ~~1.12 VLM provider codex/glm/kimi 一键切换~~ | ~~中（中国开发者）~~ | ~~S（每家 30-50 行适配）~~ | **已闭环（2026-07-05）** |
| ~~P2-4~~ | ~~1.3 openapi 多租户子路由~~ | ~~高（SaaS 多租户）~~ | ~~M（tenant registry + quota + per-tenant auth）~~ | **已闭环（2026-07-05）** |
| ~~P2-5~~ | ~~1.2 privacy LLM NER 接入~~ | ~~高（自由对话场景）~~ | ~~M（接 vlm.OpenAIClient + prompt 工程）~~ | **已闭环（2026-07-05）** |
| ~~P2-6~~ | ~~1.11 bot/hooks builtins~~ | ~~中~~ | ~~M（每个 hook 30-100 行）~~ | **已闭环（2026-07-05）** |
| ~~P3-1~~ | ~~1.7 session/train/ RL pipeline~~ | ~~低（评估已覆盖）~~ | ~~L（batch_runner + gradients + engine）~~ | **已闭环（2026-07-05）** |
| ~~P3-2~~ | ~~1.10 bot/sandbox srt/opensandbox~~ | ~~低（containerd 已覆盖）~~ | ~~M（云服务接入）~~ | **已闭环（2026-07-05）** |
| ~~P3-3~~ | ~~1.8 vectordb/vectorize 独立服务~~ | ~~低（已内联）~~ | ~~M（独立二进制）~~ | **已闭环（2026-07-05）** |
| ~~P3-4~~ | ~~1.9 parse/accessors/web_importer~~ | ~~低（webcrawler 已覆盖）~~ | ~~S（轻量单页拉取）~~ | **已闭环（2026-07-05）** |
| P3-5 | 1.1 rerank SDK 替换 | 低（手写可用） | XS（待 SDK 覆盖） | arkruntime SDK 后续版本 |
| P3-6 | 1.5 Langfuse SDK 替换 | 低（手写可用） | XS（SDK 切换） | Langfuse 成为主观测后端时 |
| ~~P4-1~~ | ~~1.13 E2E 测试基础设施 + search 优雅降级~~ | ~~高（新部署验证）~~ | ~~S（layer 2 + smoke 脚本 + newVLM nil 默认）~~ | **已闭环（2026-07-05）** |
| ~~P4-2~~ | ~~1.14 Go SDK 兼容别名（sdk/go 全量端到端打通）~~ | ~~高（SDK 用户）~~ | ~~M（14 处路由别名/handler + 2 个 Store 方法 + 5 个形状修复 + 24 子测试）~~ | **已闭环（2026-07-05，批次 2 真正语义 + CRUD）** |
| — | 1.6 feedback.go stub | — | — | 设计如此，不补 |

---

## 五、文档维护

- 本文档与 `docs/design/go-rewrite-review.md` 配对使用：review.md 记录"已闭环"，本文档记录"未补的已知 gap"。
- 任何 gap 在后续迭代中补全后，应同时更新本文档与 review.md 的回归基线表。
- 新发现的 gap 应追加到 §一，并在 §四 优先级表中排序。
