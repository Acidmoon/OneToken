# OneToken 实施计划（任务拆分与状态跟踪）

> **角色**：本文件是**项目进度的唯一真相**。设计依据见 `docs/OneToken-工程设计方案.md`（当前 v0.10），验收标准以设计文档为准，本文件负责把设计拆成可执行任务并跟踪状态。
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
| **M1** | 存储层（JSON/JSONL）/ preprocess / JSD / 校准 + Zenodo 论文复现（**不可得时按 §9.1 兜底**） | §3.2、§3.3、§3.4、§4、§9.1 | ✅ 完成（2026-08-06） |
| **M2** | 统一提供商调用层（三协议）+ 采集 + 探测 + 云端 API 参考 + 端到端审计 | §2、§5、§6、§7、§9.2 | 🔄 进行中（M2.1–M2.7 完成 2026-08-06；M2.8 验收待真实试点） |
| **M3** | 替换模拟实验 + 主辅操作点 + 报告模块 | §3.4、§9.3 | ⬜ 待办 |
| **M4** | 调度 + 告警 + 漂移管理 + CLI 完整 + 长期验收 | §9.4、§12、§15 | ⬜ 待办 |

**任务状态图例**：⬜ 待办 ｜ 🔄 进行中 ｜ ✅ 完成 ｜ ⛔ 阻塞（备注栏写明原因）

---

## 1. P0 项目脚手架

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **P0.1** 初始化仓库与 Go 模块 | ① `git init` + `.gitignore`（含 `.env`、`*.db`、构建产物）；② `go.mod`（Go 1.22+，pin 依赖版本，注意 modernc.org/sqlite 对 Go 最低版本的要求）；③ 目录骨架 `cmd/onetoken` + `internal/{config,provider,battery,collector,preprocess,detector,fingerprint,verify,calibrate,store,report}` | 无 | `go build ./...` 通过；`.gitignore` 覆盖密钥与数据库 | ✅ 2026-08-05：git init + Go 1.26.5（安装于 `~/.local/go`，代理 goproxy.cn）+ 目录骨架 + 最小 main；首次提交 475460a |
| **P0.2** 配置骨架 | ① `config/prompts.json`（40 cell 提示词模板，§3.1，与配置分离）；② `config/providers.yaml.example`（§6.1 格式：base_url 不含 `/v1`、`api_key_env` 引用、limits）；③ ~~迁移脚本骨架~~（v0.5 已废弃：JSON 存储无 SQL 迁移，改为 schema_version 兼容校验） | P0.1 | 配置可加载；模板插值校验防注入 | ✅ 2026-08-05：prompts.json 40 cell（10 任务×4 语言）+ providers 模板 + `internal/battery` 加载与防注入校验（5 单测） |
| **P0.3** 配置加载 | ① YAML + 环境变量（密钥不入文件）；② 所有阈值/规则集中配置（漂移底线 0.140、RPM/RPD、并发、超时；data 目录路径由 `store.DefaultRoot` 管理，默认 `~/.onetoken/data/`，`ONETOKEN_DATA` 覆盖）；③ **采集/审计采样参数默认值**：T=1.0 n=30、T=0 n=3、前沿（≥$5/1M 输入）n=15、审计 k∈{8,16}×n=15、输出上限 16 token（按协议命名映射 §6.2）、`store:false`；④ base_url↔密钥绑定校验骨架 | P0.2 | 单元测试：密钥禁止序列化进日志/报告；绑定校验告警；采样参数默认值断言 | ✅ 2026-08-05：`internal/config` 包（yaml.v3）：密钥注入+脱敏（String/GoString/JSON）、base_url 严格校验（url.Parse：userinfo/query/fragment 拒绝、/v1 路径段检查）、YAML 严格解析（KnownFields）、绑定校验（精确域/子域+非知名域宽松告警+本地豁免）接入 Load（Warnings）+ CLI 启动打印；Settings 集中默认值；**审查后修复**：%#v 脱敏、BindCheck 接线、环境变量污染测试、/v1 误伤、任务数/语言漂移校验、敏感头拒绝、limits 默认值、ONETOKEN_DB 死代码移除（v0.5） |

**P0 完成标准**：`go build` + `go vet` + 基础单测全绿，配置加载与安全基线骨架可用。

**范围标注**：T2 提示改写家族（设计 §13）为工程可配置项，MVP 不含，列为 v1.1——M 里程碑验收不以此为准。

---

## 2. M1 核心算法与论文复现（设计文档 §9.1）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M1.1** 存储层（JSON/JSONL，§4） | ① §4.1 目录布局（models/fingerprints/audits/responses/calibrations/drift）；② 原子写（tmp+rename）+ 目录自动创建；③ 幂等：responses JSONL 追加 + 内存 map 去重（cell+sample_idx）；④ 证据链：raw_sha256 + append-only 约定；⑤ schema_version 校验（不匹配拒绝加载）；⑥ 路径可配置（默认 ~/.onetoken/data/，ONETOKEN_DATA 覆盖） | P0.2 | 单测：原子写（无半文件残留）、JSONL 追加/读回、幂等重跑去重索引、证据链哈希、schema_version 拒绝、导入导出往返 | ✅ 2026-08-05：`internal/store` 包实现：类型定义（Model/Fingerprint/Response/Audit/Calibration/DriftEntry）、原子写（tmp+fsync+rename）、JSONL 追加（O_APPEND）、幂等索引（ResponseKey cell+idx）、sanitize 路径穿越防护、schema_version 泛型校验、导入导出往返（拷贝目录即导出）；13 单测全绿 |
| **M1.2** preprocess 归一化+分类 | ① §3.2 管线：NFC→标点剥离→大小写折叠→数字映射（阿拉伯-印度/中文）→首 token→颜色词表；② 分类 valid/invalid/refusal/empty（**无静默丢弃**）；③ 黄金样本单测（手工构造，非 Zenodo 抽取） | P0.2 | 边界测试全绿：阿拉伯-印度数字、中文数字、emoji、全角/半角、多 token 首 token 切分 | ✅ 2026-08-05：`internal/preprocess` 包：NFC（x/text/norm）→剥离标点/引号/符号（emoji）→小写→数字映射（阿印/波印/全角/中文数词解析至亿）；分类 valid/invalid(出空间或多词)/refusal(四语言核心词包含匹配)/empty；颜色/硬币跨语言词表→规范码；多词前置判定；工程约定（首 token 空格启发式、random_letter 放宽为单字母、负号剥离行为）注释在案；21 单测（含 40-cell 真实电池冒烟） |
| **M1.3** fingerprint：分布 + 基 2 JSD | ① 经验分布估计（有效样本）；② JSD：`(KL(p‖m)+KL(q‖m))/(2·ln2)`、KL 自然对数、**0·ln0=0 无平滑**；③ cell 双方 ≥10 有效样本才入平均（论文 Eq.1）；④ T=0 变体 | M1.1、M1.2 | 合成向量单测（对称性、有界 [0,1]、不相交支持）；与 scipy `jensenshannon(p,q,base=2)` **平方后**对拍（注意 sqrt 差异） | ✅ 2026-08-05：`internal/fingerprint` 包：KL（0·ln0=0，发散 +Inf）、JSD（原始标度不取 sqrt）、Normalize、Build（按 cell×温度分组、仅 valid 入指纹、valid_rate）、Distance/CellJSDs（Eq.1，双方 ≥10 过滤、无共同 cell 返 (0,0)）、T0 变体（门槛 ≥1）；16 单测全绿，含 **scipy 1.18.0 jensenshannon(base=2)² 黄金值对拍**（8 组，容差 1e-12）+ 对称性/有界性/部分不相交 0.5 手算/过滤规则；累计 82 单测全绿。**审查后修复**（正确性/安全/需求三视角）：Build 非法温度（NaN/±Inf/负）与同 cell 混合正温度 → error（ErrTemperature）、超长 Normalized（>1KB）与空串 valid 防御性跳过、全零计数 dist 过滤（Normalize 后非空）、非有限 JSD 不进均值、退化输入语义文档化+测试快照；**设计 v0.6**（Distance 签名 `(float64,int)` 与 T0 门槛留痕 §2.1/§3.3） |
| **M1.4** calibrate：ROC/AUC/EER/bootstrap | ① 分裂半 genuine / impostor 试验构造（重复奇偶切分）；② ROC/AUC/EER；③ bootstrap CI；④ (k,n,通道) 分档存储（§4 calibrations 表）；⑤ **LOO 1-NN**（自写，仅用于 M1.6 复现家族分类；设计 §2.1 标注 v1.2，此实现为复现所需，投产路径在 v1.2） | M1.1、M1.3 | 构造性单测：perfect 分类器 AUC=1、random AUC=0.5；CI 覆盖正确性抽查；1-NN 最近邻命中单测 | ✅ 2026-08-05：`internal/calibrate` 包：SplitHalves（奇偶切分）、computeROC（阈值扫描，含 (0,0)/(1,1) 端点）、AUC（曼-惠特尼 U，tie 0.5）、EER、ThresholdAtFPR（最宽松不超限）、BootstrapTPRCI（可复现种子）、Calibrate（填 store.Calibration）、LOO1NN；16 单测：构造性验收（perfect AUC=1、random AUC=0.5）、AUC 对称性、ROC 形状、EER、τ 语义（含 FPR 跳变）、bootstrap CI 覆盖+可复现、1-NN 双家族命中+无共同 cell 跳过+nil 安全、sklearn `roc_auc_score` 黄金值落库、Options 防御；累计 98 全绿。**审查后修复**（三视角）：LOO 无共同 cell 的 (0,0) 伪距离→跳过（High）、τ=−∞ 无法 JSON 序列化→Calibrate 返 nil 无效校准（High）、空输入 EER=0 假校准→nil、NaN/±Inf 得分过滤、Options NaN target 回落/NResamples 上限 1e5/Seed 默认非零、LOO nil 安全。**设计 v0.7**（§2.1 calibrate 签名、§3.4 τ_fpr 语义+τ CI 缺口 M2.5 裁决+对构造归属 M1.6）；**①④ 归属**：半指纹构建/配对/聚合与分档键填充在 M1.6 重放 harness 完成 |
| **M1.5** 前置门：pin 论文实现语义 | ① 下载 Zenodo 数据集（doi:10.5281/zenodo.21278557）+ 软件归档（doi:10.5281/zenodo.21278793），记录校验和；② 核对论文实现：是否 scipy、base 参数、**是否取 sqrt**、是否平滑、cell 过滤规则；③ 输出《语义 pin 记录》写入本文件备注；**④ 数据不可得 → 空置**（用户决策 2026-08-05）：不阻塞后续，改由用户自备权威数据（官方账号/API 采集）替代；JSD 标度维持 M1.3 既定实现（基 2 原始、无平滑）并标注"语义未 pin"，验收口径见设计 §9.1 兜底条（② 用户已确认） | 外部数据 | 语义 pin 明确：常数标度（sqrt vs 原始）确定；数据不可得时按 ④ 空置（标度即"未 pin"） | ✅ 2026-08-06：**前置门完成**（详见 `docs/OneToken-M1.5-语义pin记录.md`）：软件归档 md5 `d81de3b8…`、数据集 md5 `f2ce3fba…` 均与 Zenodo 声明一致；JSD 基 2/原始标度（不 sqrt）/无平滑**与 M1.3 一致零改动**；R pROC 做 ROC/EER；采样参数与设计一致；黄金值验证 AUC 0.971342 / EER 0.07282 / 1-NN 59.5%；数据归档 `data/zenodo/`（gitignore） |
| **M1.6** 数据重放与分层回归 | ① 重放归一化→分布→JSD 矩阵→ROC；② **cell 级中位数**：同模型分裂半 0.075、跨模型 0.489（6,564 genuine / 107 万 impostor cell 对），±5%；③ **模型对级 D 中位数**：跨 provider 0.227、噪声底线 0.140，±5%；④ 逐 cell 黄金样本对精确值校验；⑤ 全矩阵结构检查（genuine 整体 < impostor）；⑥ AUC 0.971 / EER 7.3% / 1-NN 家族分类 59.5%（±2pp）；ARI 低值（0.023）属正常；**⑦ 数据源兜底**（用户决策 2026-08-05）：Zenodo 不可得（M1.5 空置）时改用用户自备权威数据重放；论文数值项标注 N/A、验收降级为自备数据基线 + 结构检查 + 内部自洽（设计 §9.1 兜底条 ② 用户已确认） | M1.1–M1.4（M1.5 空置时豁免） | 见左列；回归脚本可复现（含数据版本哈希）；数据不可得时按 ⑦ 兜底 | ✅ 2026-08-06：**重放全部命中**（`cmd/replay`，数据=data/zenodo）：L1 分布 6572 cell 最大差 0；L2 JSD 536,649 对最大差 0.0001（toFixed 边界）；L4 cell 级 6,564 对/0.075、107 万对/0.489 精确；L5 噪声底线 0.140 精确、impostor 0.483（论文同口径 0.4832）；**L8 同模型跨 provider 56 对中位 0.2230，复现论文 0.227（±5%）**；L3 AUC 0.971318/EER 0.0729；L6 1-NN 59.509%；L7 归一化层 8 任务一致率 0.96–0.99、硬币词表对齐后 0.98、颜色 canonical 0.92；preprocess 颜色/硬币词表对齐论文（coin h/t、颜色 22 canonical） |
| **M1.7** M1 验收评审 | 汇总全部单测 + 回归报告；逐条对照设计文档 §9.1 验收项 | M1.6 | 验收清单逐项勾选；**未过项列入下一迭代** | ✅ 2026-08-06：**验收通过（11 通过 + 1 移交 M2.1 + 1 不适用，无未过项）**（详见 `docs/OneToken-M1-验收报告.md`）：98 单测全绿；§9.1 逐项核对——校验和/前置门 pin/cell 级 0.075·0.489/模型对级 0.140·0.2230/AUC 0.971318·EER 0.0729·1-NN 59.509%/结构检查/内部自洽/构造性单测全部 ✅；无未过项；M2 隐含前置（URL 单测）移交 M2.1；已知差异（refusal 超集/中文数词/颜色口径/扩展键）记录在案 |

**M1 完成标准**：前置门 pin 语义 + 分层值级回归通过 + AUC/EER/1-NN 复现 + 算法单测就绪（设计文档 §14 M1 行）；**数据不可得（M1.5 空置）时按设计 §9.1 兜底口径验收**（论文数值项 N/A + 自备数据基线/结构检查，标注"语义未 pin"）。**状态：✅ 已达成（2026-08-06，M1.7 验收通过，见 `docs/OneToken-M1-验收报告.md`；数据已获取，兜底未触发）**。

---

## 3. M2 统一调用层 + 采集 + 探测 + 双通道（设计文档 §6、§7、§9.2）

| 任务 | 子步骤 | 依赖 | 验收 | 状态 |
|---|---|---|---|---|
| **M2.1** provider 协议层 | ① 三协议适配：`/v1/responses`（`reasoning:{effort:"minimal"}`、输出过滤 `type=="message"`/`output_text`、`output_tokens_details.reasoning_tokens`）、`/v1/chat/completions`（顶层 `reasoning_effort`、**`max_tokens`（设计 §3.1 采集参数 16 token 为准；新模型 `max_completion_tokens` 映射留 M2.4 能力探测按模型切换）**、`completion_tokens_details`）、`/v1/messages`（`thinking:{type:"disabled"}`、`x-api-key`+`anthropic-version`）；② base_url 拼接 `/v1/<endpoint>`（防双 `/v1`）；③ auto 协商四步（显式优先→域名初判→能力探测：200 锁定/401-403 不降级/404-405 换协议/**400 中止**）；④ `ResponseRecord` 统一结构（含 FinishReason、ReasoningTokens） | P0.3 | **URL 构造单测**（base_url 形态矩阵）；三协议请求/响应映射单测（mock 服务器） | ✅ 2026-08-06：`internal/provider` 包（Client/Protocol/RequestParams/ResponseRecord/BuildHTTPRequest/ParseResponse/Negotiate + config.SetAPIKey）：URL 形态矩阵 12 例（尾斜杠/含 /v1/本地/子路径//v10 不误伤/query 拒绝）；三协议请求体断言（responses input+instructions+reasoning嵌套、chat 顶层 reasoning_effort+system message、anthropic 顶层 system+thinking disabled）；响应解析（reasoning item 过滤/output_text 提取/三段 usage 归一/cached_tokens 三协议透传）；协商 mock（httptest：200 锁定/404 换协议/401·403 不降级/400 中止/5xx·429 中止/三失败 undetermined/域名初判）；**21 测试全绿**；**审查后修复**：禁重定向（CheckRedirect，x-api-key 防外泄）、base_url 全路径校验（userinfo/query/fragment 拒绝，含 CLI 直传路径）、cached_tokens 补全（chat prompt_tokens_details）、5xx/429 协商中止、localhost 空密钥豁免（vLLM）、超时接线 cfg.Limits.TimeoutSec、probe 响应限量读 |
| **M2.2** provider 传输层 | ① 重试矩阵：429 Retry-After（秒/毫秒）、4xx 不重试、5xx 指数退避+jitter、最大重试与总 deadline；② 限流预算：per-provider RPM/RPD、并发 4–8（配置可覆盖）；③ 成本护栏：响应字节上限、completion 长度上限、单审计预算、超限中止记 `inconclusive`；④ SSRF：禁用重定向（`CheckRedirect` 返回 `ErrUseLastResponse`）、scheme 校验、内网/IPv6 私有段拦截、**DialContext 解析→校验→拨号**（DNS rebinding）；⑤ 日志脱敏（Authorization 永不落日志） | M2.1 | 单测：重试矩阵、限流计数、护栏触发、SSRF 拦截表 | ✅ 2026-08-06：`internal/provider` 传输层（transport/retry/ratelimit/ssrf/guardrail 五文件）：`Complete`（惰性协商+限流+重试+护栏，实现设计 §2.1 Provider 接口）；重试矩阵（429 秒/毫秒/HTTP-date、4xx 不重试、5xx 指数退避+jitter 整体封顶、Retry-After 溢出 clamp、ctx 总 deadline）；RPM/RPD 令牌桶（可注入时钟，RPD 按 UTC 日重置）；成本护栏（响应体上限、completion 上限、Budget 审计预算）；SSRF（RFC1918/环回/链路本地/CGNAT/ULA/多播/未指定/保留段补全 + `ssrf_allow` 白名单 + 解析→校验→拨号消除 TOCTOU）；敏感头黑名单扩充（api-key/x-goog-api-key 等）+ extraHeaders 禁覆盖认证头；错误体密钥回显擦洗；**43 新增测试（累计 162）**，-race/vet 全绿；**三视角审查后修复**（见 §7） |
| **M2.3** collector 并发采集 | ① worker pool（并发上限可配置）；② 幂等键（`sample_idx` + 部分唯一索引去重）；③ 可恢复续采（持久化电池进度）；④ 种子打乱 cell 顺序（速率限制亏空均匀扩散）；⑤ 进度输出 stderr / 结果 stdout（JSON） | M1.1、M2.1、M2.2 | 单测：崩溃续采不重复入库（模拟中断）；进度/结果流分离 | ✅ 2026-08-06：`internal/collector` 包（`RunBattery(ctx, provider, store, battery, cells, n, T, Options)`）：worker pool（默认 8、硬上限 256）+ 幂等续采（store.ResponseKey 索引去重，同 ID 串行重跑只补缺失）+ 种子打乱（Fisher-Yates，seed=0 确定性，调用方持久化）+ Budget 联动（超限中止返 ErrBudgetExceeded）+ 进度回调（锁内串行化保证单调，不写 stdout）+ 失败容忍/错误聚合（TaskError + 优先级：外部 ctx > 中止含根因 > 任务错误）+ worker recover；**18 新增测试（累计 181）**，-race/vet 全绿；**三视角审查后修复**（见 §7） |
| **M2.4** detector 测量有效性 | ① 推理痕迹：`reasoning_tokens > 0`（responses/chat 均显式）与 `finish_reason=="length"` 为确定性证据；退化启发式：completion token 数 1–6 正常 / 40–60 异常、可见推理轨迹；统一标记 `hidden-reasoning`；**o 系 gate：实测接受 effort 最低档且 `reasoning_tokens=0` 才可指纹化，否则按论文排除**；② T=0 一致性：探针 **n≥5**、二项检验换算阈值、分 provider 判定 → `temperature-not-honored`；③ 响应级缓存签名：T=1.0 方差崩溃 + 低延迟联合筛查（**命中=嫌疑**，论文 14/2040 均良性，需本地校准）→ `response-caching`；④ 有效率：cell 级 ≥10 有效样本门槛（论文 Eq.1）、`valid<80%` 仅模型级 QC、**refusal 率突变 → `safety-layer-change`**；⑤ **重试统计持续失败 → `unreachable`**；⑥ 有效 cell < k_min → `inconclusive`；⑦ 被标记测量**不进指纹** | M1.1、M1.2、M2.3 | 探测器 flag 单测：**与设计 §5 的 5 类 flag（hidden-reasoning / temperature-not-honored / response-caching / safety-layer-change / unreachable）逐项**各自触发/不触发场景 | ✅ 2026-08-06：`internal/detector` 包（`Screen(responses, ScreenOptions) *Result`）：5 类 flag 逐项实现——hidden-reasoning（reasoning_tokens>0/length/max_tokens/max_output_tokens 截断/退化启发式 ≥40）、temperature-not-honored（T=0 探针 n≥T0ProbeN、judged≥T0MinJudgedCells、归一化比较、ratio 兜底）、response-caching（方差崩溃+closed 空间/偏好任务豁免+可选延迟联合）、safety-layer-change（refusal 基线指针 nil=无）、unreachable（失败率 ≥0.8，失败计数经 collector.CountTaskFailures）；ValidCells/KMinCells（inconclusive 信号）+ ValidRateLow（模型级 QC）；Unknown 计数（TaskForCell 未命中可观测）；**21 新增测试（累计 211）**，-race/vet 全绿；**三视角审查后修复**（见 §7） |
| **M2.5** verify 判定 | ① 指纹距离（复用 M1.3）；② τ 匹配：按 (k,n,通道) 查校准库，无匹配档→拒绝或强制全电池校准；③ inconclusive 缓冲（\|s−τ\| 在 bootstrap CI 内） | M1.3、M1.4、M2.4 | 判定逻辑单测（pass/suspicious/inconclusive 三分支） | ✅ 2026-08-06：`internal/verify` 包（`Judge`/`MatchCalibration`/`VerifyAudit`）：三分支判定（pass ≤ τ−buf / suspicious > τ+buf / inconclusive \|s−τ\|≤buf，τ CI 缺口裁决为绝对缓冲 TauInconclusiveBuffer=0.02）；分档匹配 (k,n,scope,通道) 精确（Scope 空串只命中空档，防顺序依赖误配）；无匹配档 → ErrNoCalibration 拒绝审计；测量有效性联动（3 类 flag+unreachable → inconclusive 短路先于指纹；safety-layer-change 告警不阻断）；**fail-closed**：cellsUsed<k_min 或 0 → inconclusive（防无共同 cell 假 pass）；证据链哈希校验（RawSHA256 读侧补全）+ 入参副本回填（并发安全）+ τ 合法性（有限且 ∈[0,1]）+ CellsDetail 输出；**9 新增测试（累计 230）**，-race/vet 全绿；**三视角审查后修复**（见 §7） |
| **M2.6** reference 云端 API 单通道 | ① OfficialAPI：厂商官方 / 聚合器端点经统一调用层（§7.2，**唯一通道**——用户决策 2026-08-06：不做本地部署参考，原 LocalHost vLLM/Ollama 权重校验/同权重形状校验不再需要）；② enroll 编排：采集（collector）→ 清洗（preprocess/detector）→ 指纹构建（fingerprint.Build）→ 入库版本化（指纹 `UNIQUE(model_id,version)` + models.json 登记 + 标注来源 provider）；③ 交叉校验（可选）：同模型多 provider 建档，跨 provider 距离（M1.6 实测 0.2230）作为服务栈差异基线，不进默认判定路径 | M1.1、M2.1–M2.5 | ~~本地通道权重一致性断言~~（不适用）；enroll 版本化单测 | ✅ 2026-08-06：`internal/enroll` 包（`Enroll(ctx, Options) (*store.Fingerprint, error)`）：版本唯一性（UNIQUE 检查 + SupersededBy 版本链留痕）、双段采集（T=1.0 EnrollNT1/前沿 FrontierNT1 + T=0 EnrollNT0 独立续采 id）、测量有效性门（hidden-reasoning/response-caching → 拒绝建档；**段级 unreachable**——T1/T0 分段独立失败率，T0 段全败不被稀释）、采样参数联动校验（n1≥MinValidSamples、KMinCells≥1）、证据链（RawSHA256 已由 collector 保证 + 指纹从盘上响应构建）、入库顺序（先 models 后指纹防不可重入半状态）、Provider 结构化标注（Fingerprint/Model 新增字段）、refID 复用 store.SanitizeID（消除碰撞面）；**temperature-not-honored 门在默认 EnrollNT0=3 下不生效**（探针不足，归审计侧，代码注释留痕）；**7 新增测试（累计 242）**，-race/vet 全绿；**三视角审查后修复**（见 §7） |
| **M2.7** CLI 与端到端冒烟 | ① `enroll/probe/audit` 命令 + 直传参数（`--base-url --api-key-env --protocol [--headers]`，密钥走环境变量）；② 试点：**云端 API 建档 Qwen3-8B 等（≥2 开源模型，参考端点由用户自定）** → 同名端点 audit（**判定与校准后 τ 一致、如实报告距离**、不预设 pass；记录上游 provider 字段）；③ 三协议各跑通一次 enroll（openai-responses / anthropic / 其他云端 chat 端点）；④ impostor 冒充（不同模型）→ suspicious | M1.1、M2.5、M2.6 | 端到端冒烟记录（含实际距离、上游字段、探测器 flag） | ✅ 2026-08-06：CLI 三命令（cobra：enroll/probe/audit）+ 直传参数（密钥 env、敏感头直传拒绝、并发接线 min(flag, Limits.MaxConcurrency, 256)）；audit（--tau auto 查库/无档拒绝 + --tau 直传冒烟、seed 持久化、上游 provider 透传 X-Openrouter-Via → Audit/输出）；probe（T=0 n≥5 探针 + T=1.0 n≥CacheMinN、段级 unreachable）；**mock 端到端 9 用例**（同模型 pass/impostor suspicious/三协议 enroll/无库拒绝/上游透传/probe/未 enroll 拒绝/help<50ms）；`docs/OneToken-M2.7-冒烟记录.md`；**真实云端试点待用户密钥**（指引见冒烟记录 §3）；**三视角审查后修复**（见 §7） |
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
| **M4.3** CLI 完整与文档收尾 | ① `calibrate/report/drift` 命令；② README（安装、配置、使用、安全说明）；③ 交叉编译验证 `GOOS=windows|linux|darwin`；④ 启动耗时基准测试（含 data 目录初始化，<50ms）；⑤ **备份脚本**（每日打包 `data/` 目录 + 校验和，证据链用途，设计 §15.4） | M2.7、M3.3、M4.2 | 三平台二进制可构建；基准数据记录；备份可恢复演练 | ⬜ |
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
| R5 | ~~本地参考通道：Ollama 采样参数污染、权重版本不一致~~ → **已关闭（用户决策 2026-08-06：不采用本地部署参考，只走云端 API，§7）** | 不适用 | 关闭 |
| R6 | 探测器精度：缓存签名命中≠异常（论文 14/2040 均良性）、T=0 小样本误标 | 探针 n≥5 二项换算 + 本地校准 + 定期复审 | 开放 |
| R7 | 协议差异：o 系拒绝 effort 最低档、Azure 形态特殊 | 候选实验路径 + 实测 `reasoning_tokens=0`；Azure 移 v1.1 | 开放 |
| R8 | 成本：前沿模型建档 $1–5 为工程外推 | M2.6 实测后修正；预算按 $0.21 取 | 开放 |
| R9 | 外部依赖：Zenodo 数据可获取性 | M1.5 先行下载 + 校验和 + 固定版本；**数据不可得 → M1.5 空置（用户决策 2026-08-05），改用用户自备权威数据（官方 API/账号采集）** | 开放 |
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
| 2026-08-05 | 设计 v0.5：**存储改为分目录 JSON/JSONL**（用户评审决议，替代 SQLite）——按语义分片（响应按 audit 为 JSONL 追加）、原子写、幂等内存去重、证据链 append-only+sha256、schema_version 校验、导入导出天然；设计文档 §4/§2/§10/§15 与 AGENTS.md §4 同步 | 用户评审意见（远程仓库 + 存储讨论） |
| 2026-08-05 | **设计 v0.6**：M1.3 实现后的接口契约演进——§2.1 fingerprint 签名更新为 `Distance(a, b *Fingerprint) (float64, int)`（返回参与 cell 数支撑 k_min 判定）、§3.3 明确 T0 变体距离语义（门槛双方 ≥1、辅助信号不入判定主路径） | 审查结论（需求符合性：接口契约留痕） |
| 2026-08-05 | **设计 v0.7**：M1.4 校准算法实现后的接口契约演进——§2.1 calibrate 签名更新为 `Calibrate(genuine, impostor []float64, opts) → *store.Calibration`（分档键由调用方填充；空输入或操作点不可达返回 nil 无效校准）、§3.4 明确 τ_fpr 计算语义（FPR ≤ target 最宽松阈值；分辨率不足档位无效回退全局档）、τ CI 缺口记录（M2.5 裁决）、genuine/impostor 对构造归属 M1.6 | 审查结论（需求符合性：接口契约留痕） |
| 2026-08-05 | **M1.5 数据兜底**：① **用户决策（已确认）**：Zenodo 数据不可得时前置门**空置**、不阻塞后续，由用户自备权威数据（官方账号/API 采集）替代；② **助手推导 → 用户已确认**：M1.6 论文数值项标注 N/A、验收降级为自备数据基线+结构检查+内部自洽；JSD 标度维持 M1.3 既定实现（基 2 原始、无平滑）并标注"语义未 pin"（设计 v0.8 同步 §3.3/§9.1） | 用户意见（①）+ 用户确认（②） |
| 2026-08-05/06 | **设计 v0.9（M1.5 语义 pin）**：JSD 基 2/原始标度（不 sqrt）/无平滑与 M1.3 一致；R pROC 工具链；采样参数确认；pin 记录 docs/OneToken-M1.5-语义pin记录.md | M1.5 结果 |
| 2026-08-06 | **设计 v0.10（M1.6 重放 + 词表对齐）**：八层对拍全命中（0.075/0.489/0.140/0.483/0.227-L8/0.971318/0.0729/59.5%）；preprocess 颜色/硬币词表对齐论文 canonical（coin h/t、颜色 22 码）；0.227 复现路径（同 slug 多 provider） | M1.6 结果 + 审查 |
| 2026-08-06 | **M1.7 验收通过（M1 完成）**：98 单测 + 八层回归全命中；§9.1 验收 11 通过 + 1 移交（URL 单测→M2.1）+ 1 不适用（数据兜底）；docs/OneToken-M1-验收报告.md | M1.7 评审 |

---

## 8. 本文件更新日志

| 日期 | 变更内容 | 更新人 |
|---|---|---|
| 2026-08-05 | 计划 v1.0 建立：P0+M1–M4 拆分、风险、决策日志、更新规则 | 助手 |
| 2026-08-05 | 计划 v1.1：并入五轮审查（探测器 flag 补齐、1-NN 归属、采样参数、依赖、备份、风险来源、AGENTS.md 措辞统一） | 助手 |
| 2026-08-05 | **P0 阶段完成**：P0.1 仓库/Go 模块、P0.2 配置骨架（40-cell 提示词+battery 校验）、P0.3 config 包（密钥/阈值/绑定校验）；11 单测全绿；首次提交 475460a | 助手 |
| 2026-08-05 | **P0 六轮审查修复**：%#v 密钥脱敏（GoString）、BindCheck 接入 Load（Warnings）+ CLI 启动告警、YAML 严格解析、url.Parse 严格校验（userinfo//v1 误伤）、任务数/语言漂移校验、占位符检测扩展（裸 $/%/system_prompt）、敏感头拒绝、limits 默认值；22 单测全绿 | 助手 |
| 2026-08-05 | **设计 v0.5 + 计划 v1.2**：存储改分目录 JSON/JSONL（用户决议）；M1.1 改为 JSON 存储层；远程仓库 origin 已配置（https://github.com/Acidmoon/OneToken.git，推送待认证） | 用户评审意见 |
| 2026-08-05 | **M1.1 完成**：JSON/JSONL 存储层实现（原子写/幂等/证据链/schema_version/sanitize），13 单测；累计 35 单测全绿 | 助手 |
| 2026-08-05 | **M1.1 七轮审查修复**：sanitize Windows 保留名/非法字符/NUL、原子写统一 0644+目录 fsync、Save 复制入参、JSONL 行号错误、往返测试改 DeepEqual、schema 拒绝参数化、导入导出增强、并发追加测试；config 移除 ONETOKEN_DB 死代码；设计 §4.2 JSONL 版本豁免；文档 v0.4 残留清理（实施计划头部/P0.2/P0.3/M4.3、AGENTS.md）——36 单测全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.2 完成**：preprocess 归一化+分类（NFC/数字映射含中文数词解析/四语言拒绝/颜色硬币词表/多词判定）；21 单测；累计 57 全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.2 八轮审查修复**：multi-word 判定移至词表折叠之前（防 "blue sky" 放行）、refusal 英文词边界正则（vacant/lubricant 不再误报）、俄语 navy 无连字符变体、中文数词亿万分段文法（一万亿/一亿五千万）、refusal 模式扩充（لااستطيع/不知道/не знаю）、coin 补 head、颜色词表扩（单字中文色+高频色系归并）、小数/负号伪影与无空格中文整句文档化+测试；66 单测全绿（含 -race）；go.mod tidy（x/text 去 indirect） | 助手 |
| 2026-08-05 | **M1.3 完成**：fingerprint 包（KL/JSD 基 2 原始标度/Normalize/Build/Distance/DistanceT0）；scipy 1.18.0 对拍黄金值（清华镜像安装，8 组案例平方后容差 1e-12）；11 新增单测；累计 77 全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.3 三视角审查修复**：正确性（退化输入语义文档化、多正温度静默合并→error、prob() key 生成、scipy 模块路径注释）、安全性（非法温度/超长 Normalized/空串 valid 防御、全零计数 dist 过滤、非有限 JSD 防线、NaN 传播快照测试）、需求符合性（Distance 签名与 T0 门槛设计文档留痕 v0.6）；新增 5 单测，累计 82 全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.4 完成**：calibrate 包（SplitHalves/ROC/AUC/EER/ThresholdAtFPR/BootstrapTPRCI/Calibrate/LOO1NN）；构造性验收（perfect AUC=1、random AUC=0.5）、对称性、τ 语义（含 FPR 跳变不可达→τ=−∞）、bootstrap CI 覆盖+同种子可复现、1-NN 双家族命中；sklearn roc_auc_score 对拍一致；9 新增单测，累计 91 全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.4 三视角审查修复**：LOO 丢弃 Distance 参与数——无共同 cell 的 (0,0) 被当最近邻致家族静默错配→跳过（High）；τ=−∞ 无法 JSON 序列化（SaveCalibrations 必然失败）→Calibrate 返 nil 无效校准（High）；空输入产出 EER=0 假校准→nil；NaN/±Inf 得分过滤后重算；Options NaN target 回落/NResamples 上限 1e5/Seed 默认非零；LOO nil 安全；sklearn AUC 黄金值落库单测（替代无落库声称）；设计文档正文补齐 v0.7（§2.1/§3.4，含 τ CI 缺口 M2.5 裁决、对构造归属 M1.6）；新增 7 单测，累计 98 全绿（含 -race） | 助手 |
| 2026-08-05 | **M1.5/M1.6 数据兜底策略**：Zenodo 不可得 → M1.5 空置、不阻塞（用户决策）；JSD 标度维持既定实现并标注未 pin、M1.6 论文数值项 N/A、验收口径（助手推导，待用户复核）；设计文档同步 v0.8（§3.3/§9.1 兜底 + 文末版本说明 + §14 兜底分支 + 头部版本 bump）；实施计划同步（头部版本指针、M1 完成标准、M1.6 依赖豁免）；**审查后修复**：版本 bump 完整化、章节引用 §3.4→§3.3、验收降级归因拆分（用户决策 vs 助手推导）、§14/M1 完成标准与 §9.1 兜底对齐、循环验证与 re-pin 标注 | 助手 |
| 2026-08-05 | **M1.6 验收口径确认（用户选择「降级为自备数据基线」）**：设计 §9.1 兜底条 ② 更新为「用户已确认」、M1.6 ⑦ 与 §7 决策日志同步 | 助手 |
| 2026-08-05/06 | **M1.5 语义 pin 完成（进行中）**：软件归档下载成功并核对——JSD 基 2/原始标度（不 sqrt）/0·ln0=0 无平滑，**与 M1.3 一致零改动**；ROC/EER 用 R pROC（非 scipy）；采样参数（T=1.0 n=30、T=0 n=3、前沿 n=15、max_tokens=16、四语言）与设计 P0.3 一致；40 cell 任务一一对应；差异清单（分布/JSD 4 位舍入、refusal 词表超集、中文数词超集）记录在 `docs/OneToken-M1.5-语义pin记录.md`；数据集 52MB 下载中（本机直连 ~18KB/s，服务器被 Cloudflare 限流，软件归档经服务器中转成功） | 助手 |
| 2026-08-06 | **M1.5 完成**：数据集下载完成（续传守护方案：`-C -` 续传 + HTML 错误页检测 + systemd 用户服务；Zenodo 支持 Range、Cloudflare 间歇限流）；md5 `f2ce3fba3081f73e9908179fb2f061b6` 与声明一致；解压安全（无穿越/symlink）；**黄金值验证：AUC 0.971342 / EER 0.07282 / 1-NN 59.5% 全部命中设计声称**；数据+代码归档 `data/zenodo/`（gitignore 排除）；M1.6 对拍黄金值 = 数据集 `results/` 产物（verification.json/split-scores.json/divergence.json 等）；设计文档同步 v0.9（pin 结果 + 前置门完成标注）；三视角审查修复：family 字段实为 family_guess（0/342 无 family，R 侧 `setNames` 崩溃隐患→M1.6 用 Go 侧 LOO）、根目录 SSH 脚本清理（防凭据泄露）、服务器主机名泛化、精确化项（toFixed 三处/中文数词 0-99/refusal /i/post_reasoning 阈值） | 助手 |
| 2026-08-06 | **M1.6 完成**：`cmd/replay` 重放 harness（Go，复用 fingerprint/calibrate，L1 分布→L2 JSD→L3 ROC→L4 cell 级→L5 模型对级→L6 1-NN→L7 归一化层→**L8 同模型跨 provider**，八层对拍）全部命中论文黄金值（见任务行）；**0.227 复现**：审查发现数据集同一 slug 多 provider 记录（75/165 included 模型多 provider，gpt-4o=OpenAI+Azure、llama-3.3-70b 达 11 provider），L8 按 (model,provider) 拆指纹得 56 对同模型跨 provider 距离中位 **0.2230**（±5% 命中 0.227）；**preprocess 词表对齐论文**：硬币 canonical h/t（原 heads/tails）、颜色词表替换为论文 22 canonical（106 键 + 31 扩展 = 137），inClosedSpace 同步，单测更新；L7 归一化层量化：硬币 0.02→0.98、颜色 canonical 0.31→0.92；剩余 8 任务一致率 0.96–0.99；**颜色任务口径差异记录**：论文 normalized=原词/canonical 放 color_canon，我们 normalized=canonical（跨语言合并设计增强），M2 自用时以我们口径校准；**审查后修复**：jsd() 键排序（确定性）、pair 均值去 round4（EER 0.0729 逐位对齐）、L1 报告口径、sc.Err 防御、L6 最近邻范围、gofmt、注释过期 | 助手 |
| 2026-08-06 | **M1.7 验收通过（M1 完成）**：98 单测全绿 + 八层回归全命中；§9.1 验收 11 通过 + 1 移交（URL 单测→M2.1）+ 1 不适用（数据兜底）；`docs/OneToken-M1-验收报告.md`；已知差异（refusal 超集/中文数词/颜色口径）记录在案 | 助手 |
| 2026-08-06 | **M2.1 完成**：`internal/provider` 协议层（见任务行）；config 补 SetAPIKey（运行时密钥注入入口）；设计 §2.1 的 Provider 接口（含重试/限流）为 M2.2 最终形态，本步实现协议层中间件 Client；三视角审查修复（禁重定向/base_url 全路径校验/cached 透传/5xx 中止/本地空密钥豁免） | 助手 |
| 2026-08-06 | **M2.2 完成**：provider 传输层五文件（transport/retry/ratelimit/ssrf/guardrail）——Complete 实现设计 §2.1 Provider 接口（惰性协商+限流+重试矩阵+护栏）；重试矩阵（429 秒/毫秒/HTTP-date、4xx 不重试、指数退避+jitter 整体封顶、Retry-After 溢出 clamp）；RPM/RPD 令牌桶（可注入时钟、RPD UTC 日重置）；成本护栏（响应体上限、completion 上限、Budget）；SSRF（拦截表+`ssrf_allow` 白名单+DialContext 解析→校验→拨号）；敏感头黑名单扩充；错误体密钥回显擦洗；config 新增传输参数与 SSRFAllow；43 新增测试累计 162，-race/vet 全绿 | 助手 |
| 2026-08-06 | **M2.3 完成**：collector 包（RunBattery：worker pool 默认 8/硬上限 256、幂等续采、种子打乱、Budget 联动、进度回调、失败容忍/错误聚合、worker recover）；18 新增测试累计 181，-race/vet 全绿 | 助手 |
| 2026-08-06 | **M2.3 三视角审查修复（10 项）**：高——入库失败根因保留（abortErr+taskErrs Join）、200 响应密钥回显拒收（provider ErrSecretEchoed，TestCompleteSecretEchoRejected）；中——redactBody 先擦后截（TestRedactBodyTruncationBoundary）、Concurrency 硬上限 256、OnProgress 锁内 defer 解锁 + worker recover（TestRunBatteryWorkerPanicRecover）、seed=0 确定性（去时间回退）、cells 白名单+去重；低——T NaN/Inf 校验、同 ID 并发未定义文档、可复现性限定、进度语义文档；设计 v0.12（§2.1 RunBattery 签名具化、§6.4 密钥回显拒收） | 助手 |
| 2026-08-06 | **M2.5 完成**：verify 包（Judge/MatchCalibration/VerifyAudit：三分支判定、分档匹配含 Scope、fail-closed cellsUsed 门控、证据链哈希校验、副本回填、τ 合法性、CellsDetail）；9 新增测试累计 230，-race/vet 全绿（详见 §7 决策日志） | 助手 |
| 2026-08-06 | **M2.6 完成**：enroll 包（版本化建档：UNIQUE+SupersededBy、双段采集、测量门、段级 unreachable、Provider 结构化标注、refID 碰撞修复）；7 新增测试累计 242，-race/vet 全绿（详见 §7 决策日志） | 助手 |
| 2026-08-06 | **M2.7 完成**：CLI 三命令（enroll/probe/audit）+ 直传参数 + mock e2e 9 用例（同模型 pass/impostor suspicious/三协议 enroll/无库拒绝/上游透传/help<50ms）；冒烟记录 docs/OneToken-M2.7-冒烟记录.md；真实云端试点待用户密钥；**16 项审查修复**（上游 provider 整链、probe 缓存检测、段级 unreachable、并发接线、敏感头直传拒绝等）；新增 7 测试累计 249，-race/vet 全绿 | 助手 |
| 2026-08-06 | **参考端点用户自定（用户裁决补正）**：设计 v0.18（§7.2 参考来源不作规定、§7.3 决策表、§9.2、头部决策；撤销 v0.17 的聚合器禁作参考硬性表述）；冒烟记录 §3 与三协议 enroll 表述同步；M2.7 任务行措辞更新 | 助手 |
| 2026-08-06 | **M2.7 三视角审查修复（16 项）**：blocker——ResponseRecord.Provider 无赋值（X-Openrouter-Via 头提取）、store.Audit 缺 Provider 字段、CLI 输出缺上游字段、冒烟记录文档缺失；high——probe T1 采样 3 小于 CacheMinN 致 response-caching 不可触发（提到 10）、重复 probe 固定 ID 幂等 skip 返回空结果（新鲜 ID+时间戳）；中——audit/probe unreachable 全局比率稀释（段级判定）、Limits.MaxConcurrency 未接线（resolveConcurrency）、直传 --headers 绕黑名单（isSensitiveHeader）、audit ID 毫秒碰撞（随机后缀）、TargetChannel 硬编码（改 srcName）、指纹损坏误报不存在；低——main data 死变量、runCtx 注释、provider 与直传互斥校验、apiKeyEnv 未设置告警、probe ID 双规则碰撞（复用 store.SanitizeID）、probe T0 进度；冒烟记录含真实试点指引 | 助手 |
| 2026-08-06 | **M2.6 三视角审查修复（12 项）**：高——unreachable 段级独立判定（T0 全败不稀释）、T0 门死检测留痕 + detector T0NotJudged 修复；中——SupersededBy、入库顺序、LoadResponses 错误传播、采样联动校验、refID 规范化统一、Provider 字段；低——预算跳过 T0 段、OutputTokenCap 接线；设计 v0.16（§7.4 信任边界 + 交叉校验限制） | 助手 |
| 2026-08-06 | **参考通道单通道化（用户决策）**：设计 v0.15（§7 重写为云端 API 单通道、LocalHost 历史留痕、决策表/交叉校验/成本表/试点/§14/§16 同步）；实施计划 M2.6 改「reference 云端 API 单通道」（删 LocalHost 权重校验子步）、M2.7 试点改云端建档；风险 R5 关闭 | 助手 |
| 2026-08-06 | **M2.5 三视角审查修复（9 项）**：高——CellsUsed==0 假 pass（Distance (0,0) 被 Judge 判 pass 的 fail-open，TestVerifyAuditNoCommonCells）、Scope 分档维度缺失顺序依赖（纳入匹配键）、τ 读侧无合法性校验（篡改 calibrations.json 静默改变结论，finite01 门）；中——k_min 双口径裁决（cellsUsed 为准）、VerifyAudit 回填写回调用方切片（cloneResponses 副本 + TestVerifyAuditNoInputMutation/ConcurrentSameSlice）、RawSHA256 证据链读侧补全（TestVerifyAuditChainHashMismatch）、inconclusive 处置口径文档化（待复核不得放行，连续 N 次 fail-closed 归 M2.7）；低——Judge 注释与实现矛盾修正（s==τ+buf 归 inconclusive）、Result.CellsDetail（逐 cell JSD，§4.3 用）；设计 v0.14（§2.1/§3.4 裁决回写） | 助手 |
| 2026-08-06 | **M2.4 完成**：detector 包（Screen：5 类 flag 逐项实现 + ValidCells/KMinCells inconclusive 信号 + ValidRateLow QC + Unknown 计数 + T0 诊断字段）；21 新增测试累计 211，-race/vet 全绿（详见 §7 决策日志） | 助手 |
| 2026-08-06 | **M2.2 三视角审查修复（15 项）**：高——auto 并发协商竞态（Client.mu + Negotiate 持锁幂等，TestCompleteConcurrentAutoNegotiate）、恶意端点密钥回显擦洗（redactBody，TestCompleteSecretEchoRedacted）；中——3xx/200-解析失败不再误入重试矩阵（HTTPError 3xx 分支 + ErrBadResponse）、敏感头黑名单扩充（api-key 等 8 形态）、extraHeaders 禁覆盖认证/Content-Type/anthropic-version；低——Retry-After 溢出 clamp、jitter 整体封顶、护栏溢出保护、SSRF 保留段补全（255.255.255.255/240/4/fec0::/10 等）、truncate UTF-8 边界、Negotiate 注释诚实化+探测计入限流；设计文档同步 v0.11（§6.4/§10.1 局限与口径） | 助手 |
| 2026-08-06 | **设计 §2.1 接口边界确认（M2.1）**：Provider interface（Complete 含重试/限流/护栏）归 M2.2；M2.1 交付协议层 Client（URL/请求/解析/协商）+ ResponseRecord；Retry-After 解析归 M2.2 ①；SSRF IP 拦截/DNS rebinding 归 M2.2 ④；响应字节上限归 M2.2 ③ | M2.1 审查 |
| 2026-08-06 | **设计 v0.11（M2.2 接口与安全口径）**：Provider interface 落地为 `Client.Complete`（协议锁定→限流→重试→护栏）；SSRF 已知局限记录（系统代理绕行+内网代理需 `ssrf_allow`、环回恒放行）；超时与确定性错误（3xx/解析失败）不重试口径；敏感头黑名单扩充（api-key 等 8 形态）；`ssrf_allow` 新增配置字段；**三视角审查修复 15 项**：高——auto 并发协商竞态（Client.mu 保护 protocol、Negotiate 持锁幂等，-race 回归测试）、恶意端点密钥回显擦洗（redactBody）；中——3xx 未分类误重试、200 解析失败误重试（ErrBadResponse 不重试）、敏感头黑名单不全、extraHeaders 可覆盖认证/协议头；低——Retry-After 溢出 clamp、jitter 整体封顶、护栏溢出保护、SSRF 保留段补全、truncate UTF-8 边界、Negotiate 注释修正+探测计入限流 | 三视角审查（正确性/安全/需求符合性） |
| 2026-08-06 | **M2.3 完成（collector 并发采集）+ 接口签名演进**：设计 §2.1 `RunBattery(ctx, provider, cells, n, T)` 具化为 `RunBattery(ctx, provider.Provider, *store.Store, *battery.Battery, cells, n, T, Options)`（幂等续采需 store 索引、提示词组装需 battery、并发/预算/进度需 Options）；`[]Raw` 具化为 `[]*store.Response`（含证据链哈希与元数据）；**设计 v0.12**（§2.1 签名更新、§6.4 新增密钥回显拒收）；**三视角审查修复 10 项**：高——入库失败根因被泛化错误吞掉（abortErr+taskErrs Join 保留）、恶意端点 200 回显密钥落盘（provider 层 ErrSecretEchoed 拒收，不篡改 raw）；中——redactBody 先截后擦泄露密钥前缀（改先擦后截）、Concurrency 无上限 OOM（硬上限 256）、OnProgress 锁内 panic 死锁/杀进程（recordResult defer 解锁 + worker recover）、seed=0 时间种子不可见（改确定性 0，调用方持久化）、cells 非法/重复无校验（白名单 + 去重）；低——T NaN/Inf 无防御、同 ID 并发未定义文档化、可复现性限定并发下任务集合、进度语义文档化 | 三视角审查（正确性/安全/需求符合性） |
| 2026-08-06 | **用户裁决：参考端点由用户自定，工具不作规定（v0.18，补正 v0.17）**——enroll 参考来源**不指定、也不禁止**任何 provider（第一方或聚合器均可，用户自选信任端点）；工具只保证管线中立；设计 §7.2/§7.3/§9.2、冒烟记录 §3、CLI 示例同步 | 用户意见（补正） |
| 2026-08-06 | **M2.7 完成（CLI 与端到端冒烟）**：cobra 三命令 + 直传参数 + mock e2e 9 用例（同模型 pass / impostor suspicious / 三协议 enroll / 无库拒绝 / 上游透传）；**设计外扩展留痕**：`--tau` 直传阈值（TauOverride，冒烟/临时，verify.Options；正常审计 auto 查库）、上游 provider 字段数据源（X-Openrouter-Via 等响应头，§6.4/§9.2 落实）、TargetChannel 分档键=被审计端点 provider 名、safety-layer-change 需 refusal 基线（指纹未持久化，下迭代接线）；**三视角审查修复 16 项**：blocker——上游 provider 字段整链缺失（provider 头提取 → Audit 字段 → 输出）、冒烟记录文档缺失；high——probe 缓存检测结构性不可触发（T1 采样提到 ≥CacheMinN）、重复 probe 幂等 skip 空结果（新鲜 ID）；中——段级 unreachable（audit/probe 对齐 enroll）、Limits.MaxConcurrency 未接线（resolveConcurrency）、直传 --headers 绕过敏感头黑名单（isSensitiveHeader 拒绝）、audit ID 毫秒碰撞（随机后缀）、TargetChannel 硬编码、指纹损坏错误误导；低——data 死变量、runCtx 注释、互斥校验、apiKeyEnv 未设置告警、probe ID 双规则（复用 store.SanitizeID）、T0 段进度 | 三视角审查（正确性/安全/需求符合性） |
| 2026-08-06 | **M2.6 完成（enroll 云端 API 建档）+ 设计 v0.16**：`Enroll` 版本化编排（UNIQUE+SupersededBy、双段采集、测量门、段级 unreachable、入库顺序、Provider 结构化标注）；**设计 §7.4 信任边界声明**（参考源被顶替 → 指纹污染不可检出，属信任模型边界；指纹记录来源 provider/时间支持溯源）；**交叉校验能力限制**（同 modelID 单文件覆盖，多 provider 需不同 modelID 或 M4.2 布局）；**三视角审查修复 12 项**：高——unreachable 分子分母口径（T0 段全败被 T1 稀释 → 段级独立判定）、temperature-not-honored 在 enroll 死检测（EnrollNT0=3<T0ProbeN=5 → 门不生效留痕 + detector T0NotJudged 死字段修复）；中——SupersededBy 版本链留痕、入库顺序（先 models 后指纹防不可重入）、LoadResponses 错误吞掉、采样参数联动校验（n1≥MinValidSamples）、refID 双规则碰撞（复用 store.SanitizeID）、Provider 结构化标注（Fingerprint/Model 字段）；低——预算中止跳过 T0 段、OutputTokenCap 接线、并发 Enroll 未定义文档 | 三视角审查（正确性/安全/需求符合性） |
| 2026-08-06 | **用户决策：参考通道单通道化（云端 API）**——不做本地部署模型参考（原 §7.2 LocalHost vLLM/Ollama 不采用），参考指纹只走云端 API（厂商官方/聚合器，经统一调用层）；设计 v0.15（§7 重写、§9.2/§14/§16 同步）、M2.6 任务行裁剪（删本地权重校验）、M2.7 试点改云端建档、风险 R5 关闭 | 用户意见 |
| 2026-08-06 | **M2.5 完成（verify 判定）+ 设计 v0.14**：`VerifyAudit` 具化设计 §2.1（端到端 Verify 含采集归 M2.7）；**裁决（设计 §3.4 回写）**：τ CI 缺口 → 绝对缓冲 TauInconclusiveBuffer=0.02（需本地校准）；k_min 双口径 → 以 B′ cellsUsed 为准 + fail-closed（cellsUsed=0 判 inconclusive）；Scope 纳入分档键（空串只命中空档）；**三视角审查修复 9 项**：高——CellsUsed=0 假 pass（fail-open → 门控）、MatchCalibration 缺 Scope 顺序依赖（纳入匹配键）、τ 无合法性校验（有限且 ∈[0,1]）；中——k_min 口径裁决、回填写回调用方切片（副本回填+并发 -race 测试）、RawSHA256 读侧补全（证据链篡改检测）、inconclusive 逃逸（处置口径文档化，连续 N 次 fail-closed 归 M2.7）；低——Judge 边界注释修正、CellsDetail 输出（§4.3 audits 用）；ErrNoCalibration 短路遮蔽注记 | 三视角审查（正确性/安全/需求符合性） |
| 2026-08-06 | **M2.4 完成（detector 测量有效性）+ 设计 v0.13**：`Screen(responses, ScreenOptions) *Result` 实现设计 §2.1（入参原始响应、出参 5 类 Flags+统计，cleaned 由调用方据 Flags 过滤）；**口径留痕**（设计 §5 末段）：截断信号跨协议（length/max_tokens/max_output_tokens）、退化启发式可达性受护栏约束（护栏归因 M2.5）、T=0 实现为自洽性（参考比对降级，主距离兜底）、偏好任务/closed 空间缓存豁免、延迟联合默认禁用（可被端点欺骗）、unreachable 失败计数经 `collector.CountTaskFailures`、safety 基线指针化、o 系 gate 归属 M2.7 能力探测、k_min 双口径 M2.5 裁决；**三视角审查修复 12 项**：高——Anthropic max_tokens 截断漏报（匹配集合扩充）；中——偏好任务缓存误报（favorite 前缀豁免+closed 空间校准）、T0 ratio>1/NaN 兜底、judged 门槛（T0MinJudgedCells=3）、T0 归一化比较、RefusalBaseline 零值误用（改指针）、TaskForCell 未命中静默（Unknown 计数）、unreachable 失败统计生产者（CountTaskFailures 修复 joinError As 吞兄弟）、ValidRateQC 死配置接线（ValidRateLow）；低——7-39 灰区注释、CompletionTokens 含 reasoning token 口径注记 | 三视角审查（正确性/安全/需求符合性） |

---

*下次更新本文件时：勾选完成状态 + 填日期/备注 + 在 §7/§8 追加记录；状态变更或决策变更都必须留痕。*
