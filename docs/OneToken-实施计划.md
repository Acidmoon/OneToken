# OneToken 实施计划（任务拆分与状态跟踪）

> **角色**：本文件是**项目进度的唯一真相**。设计依据见 `docs/OneToken-工程设计方案.md`（当前 v0.4），验收标准以设计文档为准，本文件负责把设计拆成可执行任务并跟踪状态。
> **更新规则**：**每次完成任何实质性工作后必须更新本文件**（勾选状态、填日期与备注、按 AGENTS.md §3.1/§3.2 追加 §7 决策日志与 §8 更新日志）；收到反馈或评审意见后，先更新相关文档（本文件与设计文档），再动手改代码。规则详见 `AGENTS.md`。

---

## 0. 阶段总览与依赖图

```
P0 脚手架 ──→ M1 核心算法与论文复现 ──→ M2 统一调用层+采集+探测+双通道 ──→ M3 替换模拟与操作点 ──→ M4 持续运行与收尾
   (无依赖)      (依赖 P0)                 (依赖 P0/M1)                     (依赖 M2)               (依赖 M2/M3)
```

| 阶段 | 内容 | 设计文档依据 | 状态 |
|---|---|---|---|
| **P0** | Go 项目脚手架、配置骨架 | §10、§14 | ✅ 完成（2026-08-05） |
| **M1** | store / preprocess / JSD / 校准 + Zenodo 论文复现 | §3.2、§3.3、§3.4、§4、§9.1 | ⬜ 待办 |
| **M2** | 统一提供商调用层（三协议）+ 采集 + 探测 + 双通道参考 + 端到端审计 | §2、§5、§6、§7、§9.2 | ⬜ 待办 |
| **M3** | 替换模拟实验 + 主辅操作点 + 报告模块 | §3.4、§9.3 | ⬜ 待办 |
| **M4** | 调度 + 告警 + 漂移管理 + CLI 完整 + 长期验收 | §9.4、§12、§15 | ⬜ 待办 |

**任务状态图例**：⬜ 待办 ｜ 🔄 进行中 ｜ ✅ 完成 ｜ ⛔ 阻塞（备注栏写明原因）

---

## 1. P0 项目脚手架

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **P0.1** 初始化仓库与 Go 模块 | ① `git init` + `.gitignore`（含 `.env`、`*.db`、构建产物）；② `go.mod`（Go 1.22+，pin 依赖版本，注意 modernc.org/sqlite 对 Go 最低版本的要求）；③ 目录骨架 `cmd/onetoken` + `internal/{config,provider,battery,collector,preprocess,detector,fingerprint,verify,calibrate,store,report}` | 无 | `go build ./...` 通过；`.gitignore` 覆盖密钥与数据库 | ✅ 2026-08-05：git init + Go 1.26.5（安装于 `~/.local/go`，代理 goproxy.cn）+ 目录骨架 + 最小 main；首次提交 475460a |
| **P0.2** 配置骨架 | ① `config/prompts.json`（40 cell 提示词模板，§3.1，与配置分离）；② `config/providers.yaml.example`（§6.1 格式：base_url 不含 `/v1`、`api_key_env` 引用、limits）；③ 迁移脚本骨架（幂等） | P0.1 | 配置可加载；模板插值校验防注入 | ✅ 2026-08-05：prompts.json 40 cell（10 任务×4 语言）+ providers 模板 + `internal/battery` 加载与防注入校验（5 单测）+ `internal/store/migrations.go` 骨架 |
| **P0.3** 配置加载 | ① YAML + 环境变量（密钥不入文件）；② 所有阈值/规则集中配置（漂移底线 0.140、RPM/RPD、并发、超时、**db 路径默认 `~/.onetoken/onetoken.db`**）；③ **采集/审计采样参数默认值**：T=1.0 n=30、T=0 n=3、前沿（≥$5/1M 输入）n=15、审计 k∈{8,16}×n=15、输出上限 16 token（按协议命名映射 §6.2）、`store:false`；④ base_url↔密钥绑定校验骨架 | P0.2 | 单元测试：密钥禁止序列化进日志/报告；绑定校验告警；采样参数默认值断言 | ✅ 2026-08-05：`internal/config` 包（yaml.v3）：密钥注入+脱敏（String/GoString/JSON）、base_url 严格校验（url.Parse：userinfo/query/fragment 拒绝、/v1 路径段检查）、YAML 严格解析（KnownFields）、绑定校验（精确域/子域+非知名域宽松告警+本地豁免）接入 Load（Warnings）+ CLI 启动打印；Settings 集中默认值 + ONETOKEN_DB 覆盖；**审查后修复**：%#v 脱敏、BindCheck 接线、环境变量污染测试、/v1 误伤、任务数/语言漂移校验、敏感头拒绝、limits 默认值（22 单测全绿） |

**P0 完成标准**：`go build` + `go vet` + 基础单测全绿，配置加载与安全基线骨架可用。

**范围标注**：T2 提示改写家族（设计 §13）为工程可配置项，MVP 不含，列为 v1.1——M 里程碑验收不以此为准。

---

## 2. M1 核心算法与论文复现（设计文档 §9.1）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M1.1** store 层 | ① §4 全部表 + **两条部分唯一索引**（`idx_resp_idem_audit` / `idx_resp_idem_fp`）+ `idx_audits_daily` 独立索引；② 连接约定：`PRAGMA foreign_keys=ON + journal_mode=WAL + busy_timeout=5000`；③ 迁移（`schema_version`）；④ UTC `Z` 后缀时间戳约定 | P0.2 | 外键/唯一约束单测（含 NULL 语义验证）；raw 行只 INSERT 不 UPDATE | ⬜ |
| **M1.2** preprocess 归一化+分类 | ① §3.2 管线：NFC→标点剥离→大小写折叠→数字映射（阿拉伯-印度/中文）→首 token→颜色词表；② 分类 valid/invalid/refusal/empty（**无静默丢弃**）；③ 黄金样本单测（Zenodo 数据抽取） | P0.2 | 边界测试全绿：阿拉伯-印度数字、中文数字、emoji、全角/半角、多 token 首 token 切分 | ⬜ |
| **M1.3** fingerprint：分布 + 基 2 JSD | ① 经验分布估计（有效样本）；② JSD：`(KL(p‖m)+KL(q‖m))/(2·ln2)`、KL 自然对数、**0·ln0=0 无平滑**；③ cell 双方 ≥10 有效样本才入平均（论文 Eq.1）；④ T=0 变体 | M1.1、M1.2 | 合成向量单测（对称性、有界 [0,1]、不相交支持）；与 scipy `jensenshannon(p,q,base=2)` **平方后**对拍（注意 sqrt 差异） | ⬜ |
| **M1.4** calibrate：ROC/AUC/EER/bootstrap | ① 分裂半 genuine / impostor 试验构造（重复奇偶切分）；② ROC/AUC/EER；③ bootstrap CI；④ (k,n,通道) 分档存储（§4 calibrations 表）；⑤ **LOO 1-NN**（自写，仅用于 M1.6 复现家族分类；设计 §2.1 标注 v1.2，此实现为复现所需，投产路径在 v1.2） | M1.1、M1.3 | 构造性单测：perfect 分类器 AUC=1、random AUC=0.5；CI 覆盖正确性抽查；1-NN 最近邻命中单测 | ⬜ |
| **M1.5** 前置门：pin 论文实现语义 | ① 下载 Zenodo 数据集（doi:10.5281/zenodo.21278557）+ 软件归档（doi:10.5281/zenodo.21278793），记录校验和；② 核对论文实现：是否 scipy、base 参数、**是否取 sqrt**、是否平滑、cell 过滤规则；③ 输出《语义 pin 记录》写入本文件备注 | 外部数据 | 语义 pin 明确：常数标度（sqrt vs 原始）确定 | ⬜ |
| **M1.6** 数据重放与分层回归 | ① 重放归一化→分布→JSD 矩阵→ROC；② **cell 级中位数**：同模型分裂半 0.075、跨模型 0.489（6,564 genuine / 107 万 impostor cell 对），±5%；③ **模型对级 D 中位数**：跨 provider 0.227、噪声底线 0.140，±5%；④ 逐 cell 黄金样本对精确值校验；⑤ 全矩阵结构检查（genuine 整体 < impostor）；⑥ AUC 0.971 / EER 7.3% / 1-NN 家族分类 59.5%（±2pp）；ARI 低值（0.023）属正常 | M1.1–M1.5 | 见左列；回归脚本可复现（含数据版本哈希） | ⬜ |
| **M1.7** M1 验收评审 | 汇总全部单测 + 回归报告；逐条对照设计文档 §9.1 验收项 | M1.6 | 验收清单逐项勾选；**未过项列入下一迭代** | ⬜ |

**M1 完成标准**：前置门 pin 语义 + 分层值级回归通过 + AUC/EER/1-NN 复现 + 算法单测就绪（设计文档 §14 M1 行）。

---

## 3. M2 统一调用层 + 采集 + 探测 + 双通道（设计文档 §6、§7、§9.2）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M2.1** provider 协议层 | ① 三协议适配：`/v1/responses`（`reasoning:{effort:"minimal"}`、输出过滤 `type=="message"`/`output_text`、`output_tokens_details.reasoning_tokens`）、`/v1/chat/completions`（顶层 `reasoning_effort`、`max_completion_tokens`、`completion_tokens_details`）、`/v1/messages`（`thinking:{type:"disabled"}`、`x-api-key`+`anthropic-version`、`retry-after-ms`）；② base_url 拼接 `/v1/<endpoint>`（防双 `/v1`）；③ auto 协商四步（显式优先→域名初判→能力探测：200 锁定/401-403 不降级/404-405 换协议/**400 中止**）；④ `ResponseRecord` 统一结构（含 FinishReason、ReasoningTokens） | P0.3 | **URL 构造单测**（base_url 形态矩阵）；三协议请求/响应映射单测（mock 服务器） | ⬜ |
| **M2.2** provider 传输层 | ① 重试矩阵：429 Retry-After（秒/毫秒）、4xx 不重试、5xx 指数退避+jitter、最大重试与总 deadline；② 限流预算：per-provider RPM/RPD、并发 4–8（配置可覆盖）；③ 成本护栏：响应字节上限、completion 长度上限、单审计预算、超限中止记 `inconclusive`；④ SSRF：禁用重定向（`CheckRedirect` 返回 `ErrUseLastResponse`）、scheme 校验、内网/IPv6 私有段拦截、**DialContext 解析→校验→拨号**（DNS rebinding）；⑤ 日志脱敏（Authorization 永不落日志） | M2.1 | 单测：重试矩阵、限流计数、护栏触发、SSRF 拦截表 | ⬜ |
| **M2.3** collector 并发采集 | ① worker pool（并发上限可配置）；② 幂等键（`sample_idx` + 部分唯一索引去重）；③ 可恢复续采（持久化电池进度）；④ 种子打乱 cell 顺序（速率限制亏空均匀扩散）；⑤ 进度输出 stderr / 结果 stdout（JSON） | M1.1、M2.1、M2.2 | 单测：崩溃续采不重复入库（模拟中断）；进度/结果流分离 | ⬜ |
| **M2.4** detector 测量有效性 | ① 推理痕迹：`reasoning_tokens > 0`（responses/chat 均显式）与 `finish_reason=="length"` 为确定性证据；退化启发式：completion token 数 1–6 正常 / 40–60 异常、可见推理轨迹；统一标记 `hidden-reasoning`；**o 系 gate：实测接受 effort 最低档且 `reasoning_tokens=0` 才可指纹化，否则按论文排除**；② T=0 一致性：探针 **n≥5**、二项检验换算阈值、分 provider 判定 → `temperature-not-honored`；③ 响应级缓存签名：T=1.0 方差崩溃 + 低延迟联合筛查（**命中=嫌疑**，论文 14/2040 均良性，需本地校准）→ `response-caching`；④ 有效率：cell 级 ≥10 有效样本门槛（论文 Eq.1）、`valid<80%` 仅模型级 QC、**refusal 率突变 → `safety-layer-change`**；⑤ **重试统计持续失败 → `unreachable`**；⑥ 有效 cell < k_min → `inconclusive`；⑦ 被标记测量**不进指纹** | M1.1、M1.2、M2.3 | 探测器 flag 单测：**与设计 §5 的 5 类 flag（hidden-reasoning / temperature-not-honored / response-caching / safety-layer-change / unreachable）逐项**各自触发/不触发场景 | ⬜ |
| **M2.5** verify 判定 | ① 指纹距离（复用 M1.3）；② τ 匹配：按 (k,n,通道) 查校准库，无匹配档→拒绝或强制全电池校准；③ inconclusive 缓冲（\|s−τ\| 在 bootstrap CI 内） | M1.3、M1.4、M2.4 | 判定逻辑单测（pass/suspicious/inconclusive 三分支） | ⬜ |
| **M2.6** reference 双通道 | ① LocalHost：**vLLM 优先**（`top_p=1.0/top_k=-1/repetition_penalty=1.0` 显式传）、Ollama 受限（锁采样参数 + **同权重形状校验**：vLLM vs Ollama 各 100+ 样本 JSD<0.05）、权重哈希记录；② OfficialAPI：厂商官方端点经统一调用层；③ enroll 编排（采集→清洗→入库→版本化 `UNIQUE(model_id,version)`）；④ 双源交叉校验（可选） | M1.1、M2.1–M2.4 | 本地通道权重一致性断言；enroll 版本化单测 | ⬜ |
| **M2.7** CLI 与端到端冒烟 | ① `enroll/probe/audit` 命令 + 直传参数（`--base-url --api-key-env --protocol [--headers]`，密钥走环境变量）；② 试点：本地 Qwen3-8B 建档（≥2 开源模型）→ OpenRouter 同名端点 audit（**判定与校准后 τ 一致、如实报告距离**、不预设 pass；记录上游 provider 字段）；③ 三协议各跑通一次 enroll（local-chat / openai-responses / anthropic）；④ impostor 冒充（不同模型）→ suspicious | M1.1、M2.5、M2.6 | 端到端冒烟记录（含实际距离、上游字段、探测器 flag） | ⬜ |
| **M2.8** M2 验收评审 | 逐条对照设计文档 §9.2：探测器 flag 断言、三协议 enroll、≥2 开源模型、端到端判定一致性 | M2.7 | 验收清单逐项勾选；未过项入下一迭代 | ⬜ |

**M2 完成标准**：三协议可用 + 端到端审计链路通 + 探测器全 flag 正确（设计文档 §14 M2 行）。

---

## 4. M3 替换模拟与操作点（设计文档 §9.3）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M3.1** 假目标构造 | ① 本地可控：vLLM 加载 AWQ/GPTQ 量化权重、旧版 checkpoint（记录权重哈希）；② 公网：不同模型端点；③ 本地-公网结果**分开报告**（本地无服务栈噪声、结果偏乐观） | M2.7 | 假目标清单与构造脚本可复现 | ⬜ |
| **M3.2** 主辅操作点评估 | ① 真/假目标各 **≥8**；② 每端点重复审计 **≥5 次**取判定序列，报告二项 CI；③ **主评估点 τ_fpr1**：TPR ≥ T（T 由 M1 校准数据预先确定并记录，预计 60–80%，如实报告 CI）；④ **辅评估点 τ_fpr5**：TPR ≥ 90%；⑤ 同端点重复审计 verdict 一致性 ≥ 80% | M3.1、M1.4 | 评估报告：两个操作点的 FPR/TPR 对 + CI；数值先定指标后定值 | ⬜ |
| **M3.3** 报告模块 | ① 单端点报告（逐 cell JSD 明细、QC flags）；② 距离矩阵热力图；③ UPGMA 聚类图（**v1.1**，复刻论文 Fig.2）；④ HTML 输出经 `html/template` 默认转义（模型输出只进文本节点，SVG 结构内部常量生成） | M2.7 | 报告样例 + XSS 注入用例（恶意 raw 文本被转义） | ⬜ |
| **M3.4** M3 验收评审 | 逐条对照设计文档 §9.3 | M3.2、M3.3 | 验收清单逐项勾选 | ⬜ |

**M3 完成标准**：主辅操作点评估完成 + 报告可用（设计文档 §14 M3 行）。

---

## 5. M4 持续运行与收尾（设计文档 §9.4、§12、§15）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M4.1** 调度与告警 | ① cron 示例（**UTC 显式**，审计/刷新/校准错峰，§15 模板）；② 告警规则配置化（阈值、漂移、QC flags）；③ 成本护栏联动（超预算告警） | M2.7 | 调度 dry-run 正确；告警触发单测 | ⬜ |
| **M4.2** 漂移管理 | ① 参考指纹 TTL 30 天自动刷新（`superseded_by` 链）；② 趋势监测：近 5 次均值 > 0.140 且递增 → `stale`；③ `-latest` 别名多版本比对（输出"更像哪个版本"） | M2.7、M4.1 | 漂移判定单测；刷新保留历史版本 | ⬜ |
| **M4.3** CLI 完整与文档收尾 | ① `calibrate/report/drift` 命令；② README（安装、配置、使用、安全说明）；③ 交叉编译验证 `GOOS=windows|linux|darwin`；④ 启动耗时基准测试（含 DB 打开与迁移，<50ms）；⑤ **备份脚本**（每日备份 db + 校验和，证据链用途，设计 §15.4） | M2.7、M3.3、M4.2 | 三平台二进制可构建；基准数据记录；备份可恢复演练 | ⬜ |
| **M4.4** M4 长期验收 | ① 10 端点 × 每日 × 14 天 ≈ 140 次审计，误报数 ∈ τ_fpr1 对应二项 CI（≈0–4 次）；② 人为注入替换**全部捕获**（注入协议先行定义：注入什么、多频繁、谁判定捕获）；③ 每 7 天更新一次本计划文档的验收进度 | M4.1–M4.3 | 14 天区间验收记录；注入捕获率 100% | ⬜ |

**M4 完成标准**：设计文档 §14 M4 行验收项达成，交付 MVP。

---

## 6. 全局风险跟踪

**来源说明**：R1–R7、R10 对应设计文档 §16 第 1–5、7–10 条；R8 源自 §11 成本（预算按 $0.21 实测均值）；R9 源自 §9.1 前置门（Zenodo 数据可用性）。设计 §16 第 6 条（单聚合器结论不泛化）并入 R4，第 9 条（协议差异收敛）并入 R7。

| ID | 风险 | 缓解 | 状态 |
|---|---|---|---|
| R1 | 参考指纹硬约束：无官方 API/开源权重的模型只能互比线索 | §3.5 降级路径（v1.2） | 开放 |
| R2 | 错误率本质：τ 小样本 CI 宽、误报优先→漏报 | bootstrap CI 如实报告；inconclusive 缓冲 + 重复审计 | 开放 |
| R3 | 指纹漂移：模型更新导致验证窗口错位 | TTL 30 天 + 趋势监测（M4.2） | 开放 |
| R4 | 跨通道分布差异（中位 0.227）+ OpenRouter 多上游路由 | 同通道比对优先 + 校准后 τ；记录上游 provider | 开放 |
| R5 | 本地参考通道：Ollama 采样参数污染、权重版本不一致 | vLLM 优先 + 同权重形状校验 + 权重哈希（M2.6） | 开放 |
| R6 | 探测器精度：缓存签名命中≠异常（论文 14/2040 均良性）、T=0 小样本误标 | 探针 n≥5 二项换算 + 本地校准 + 定期复审 | 开放 |
| R7 | 协议差异：o 系拒绝 effort 最低档、Azure 形态特殊 | 候选实验路径 + 实测 `reasoning_tokens=0`；Azure 移 v1.1 | 开放 |
| R8 | 成本：前沿模型建档 $1–5 为工程外推 | M2.6 实测后修正；预算按 $0.21 取 | 开放 |
| R9 | 外部依赖：Zenodo 数据可获取性 | M1.5 先行下载 + 校验和 + 固定版本 | 开放 |
| R10 | 合规边界：仅付费配额内普通请求、统计偏差报告 | 设计文档 §16 第 5 条 | 开放 |

---

## 7. 决策与变更日志（每次变更设计/计划必须追加）

| 日期 | 版本 | 决策/变更 | 关联 |
|---|---|---|---|
| 2026-08-05 | 设计 v0.1 | 初始设计（Python 技术栈） | — |
| 2026-08-05 | 设计 v0.2 | 并入两轮对抗式审查（scipy 单位陷阱、验收统计、幂等键等） | — |
| 2026-08-05 | 设计 v0.3 | 用户评审：**统一提供商调用层**（任意 BaseURL+Key、三协议）；**语言改 Go** | 用户意见 1/2 |
| 2026-08-05 | 设计 v0.4 | 并入四轮审查：effort 取值修正（minimal 候选路径）、base_url 不含 `/v1`、SQLite 部分唯一索引、JSD 无平滑 + 前置门 pin 标度、CheckRedirect 显式配置等 | 审查结论 |
| 2026-08-05 | 计划 v1.0 | 建立本实施计划（P0+M1–M4 任务拆分）与 AGENTS.md 工作协议 | 用户要求 |
| 2026-08-05 | 计划 v1.1 | 并入五轮审查：探测器 flag 补齐（5 类对齐设计 §5）、LOO 1-NN 归属 M1.4、采样参数/db 路径入 P0.3、M2 依赖补 M1.1、备份入 M4.3、T2 改写标注 v1.1、风险来源说明；同步修正设计文档脚注（v0.4/四轮） | 审查结论 |
| 2026-08-05 | P0 完成 | **P0.1–P0.3 全部完成**（见任务表）；Go 1.26.5 安装于 `~/.local/go`（go.dev 大文件被网络干扰，改用阿里云镜像），模块代理 goproxy.cn；首次提交 475460a | 用户批准启动 |

---

## 8. 本文件更新日志

| 日期 | 变更内容 | 更新人 |
|---|---|---|
| 2026-08-05 | 计划 v1.0 建立：P0+M1–M4 拆分、风险、决策日志、更新规则 | 助手 |
| 2026-08-05 | 计划 v1.1：并入五轮审查（探测器 flag 补齐、1-NN 归属、采样参数、依赖、备份、风险来源、AGENTS.md 措辞统一） | 助手 |
| 2026-08-05 | **P0 阶段完成**：P0.1 仓库/Go 模块、P0.2 配置骨架（40-cell 提示词+battery 校验）、P0.3 config 包（密钥/阈值/绑定校验）；11 单测全绿；首次提交 475460a | 助手 |
| 2026-08-05 | **P0 六轮审查修复**：%#v 密钥脱敏（GoString）、BindCheck 接入 Load（Warnings）+ CLI 启动告警、YAML 严格解析、url.Parse 严格校验（userinfo//v1 误伤）、任务数/语言漂移校验、占位符检测扩展（裸 $/%/system_prompt）、敏感头拒绝、limits 默认值；22 单测全绿 | 助手 |

---

*下次更新本文件时：勾选完成状态 + 填日期/备注 + 在 §7/§8 追加记录；状态变更或决策变更都必须留痕。*
