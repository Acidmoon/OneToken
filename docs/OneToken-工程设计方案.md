# OneToken 工程设计方案：LLM 端点真伪检测系统

> **依据论文**：《One Token Is Enough: Fingerprinting and Verifying Large Language Models from Single-Token Output Distributions》（arXiv:2607.10252，Tomáš Bruckner）
>
> **文档状态**：v0.10（并入 M1.6 重放结果与 0.227 事实澄清、preprocess 颜色/硬币词表对齐论文）
> **决策记录**：
> - 实现语言：**Go**（IO 密集场景，启动毫秒级、单二进制、goroutine 并发天然适配批量采集；开发/迭代快）——用户拍板。
> - **统一提供商调用层为系统核心**：任意 BaseURL + API Key 即可请求，三种协议适配（OpenAI Responses / OpenAI chat completions 兼容 / Anthropic messages），参考注册（enroll）与待审核模型（audit）共用此层——用户评审意见 1。
> - **存储：分目录 JSON/JSONL 文件存储，替代 SQLite**（用户评审决议 v0.5）——单用户 CLI、数据按语义分片、导入导出与开放共享；响应按审计分片为 JSONL 追加，避免单大文件全量读写；详见 §4。
> - 参考指纹通道：能力层双通道——开源模型本地部署（LocalHost）与厂商官方 API（OfficialAPI）；每模型默认按类型选单一通道（开源→本地优先，闭源→官方 API），同模型双源交叉校验为可选配置（§7.5）。
> - 首个试点目标：本地部署 Qwen3-8B 建档 → OpenRouter 同名端点审计（§9.2）。
> - 操作点：默认误报优先（τ 对应 FPR≈1%），附 τ_fpr5 辅评估点；阈值按 (k, n, 通道) 分档存储（§3.4、§4）。
> - **M1.5 前置门 pin 结果（v0.9）**：论文软件归档（Zenodo 21278793）核对完成——JSD 基 2、**原始标度（不取 sqrt）**、0·ln0=0 无平滑、cell 双方 ≥10 有效样本、R pROC 做 ROC/EER；与 Go 实现（M1.3/M1.4）完全一致，零改动；采样参数（T=1.0 n=30、T=0 n=3、前沿 n=15、max_tokens=16、四语言）与 §P0.3 一致；详见 `docs/OneToken-M1.5-语义pin记录.md`。

---

## 1. 目标与范围

### 1.1 系统目标

构建一个工程可落地的**黑盒 LLM 端点真伪检测系统**：给定"某提供商的某模型端点"，通过单 token 行为指纹验证其**实际服务的模型是否与声明的模型一致**，检出以下欺骗行为：

- **模型替换**：用更便宜的模型顶替声明的模型（T1）；
- **量化顶替**：用激进量化/蒸馏变体代替全精度版本；
- **版本回退**：服务旧版本 checkpoint；
- **跨 provider 行为漂移**：同一模型在不同服务商上分布显著偏离（潜在部署异常）。

### 1.2 系统边界（做什么/不做什么）

| 做（MVP 范围） | 不做（明确范围外） |
|---|---|
| 单 token 指纹采集、存储、验证 | 对抗性对手（T3 仿冒）的绝对保证 |
| 统一提供商调用层（任意 BaseURL + Key，三协议） | 水印/白盒合作方案 |
| 参考指纹双通道获取（开源本地 + 官方 API） | 多模型混合路由的逐请求溯源 |
| 阈值校准（genuine/impostor、ROC/EER、bootstrap CI） | 强制隐藏推理模型（o-series 等）的直接指纹化* |
| 测量有效性探测（温度、推理、缓存签名） | 端到端鉴权/权限体系（v2 讨论） |
| 调度、告警、报告、指纹漂移管理 | — |

\* 论文同样排除强制隐藏推理端点；Responses API 的 `reasoning_tokens` 显式统计提供确定性证据，`reasoning.effort` 最低档（minimal/low）为**候选实验路径**——仅当实测目标模型接受最低档且 usage 显示 `reasoning_tokens=0` 时才可指纹化，否则按论文排除（o 系实测拒绝 "none"，该取值不作为任何协议的主取值）；post-reasoning 通道指纹化列为未来扩展。

### 1.3 核心设计原则（第一性原理）

1. **指纹的唯一要求是"条件分布可采样"**——不需要 logits、权重或合作；**协议层统一、行为层按 provider 校准**（论文实测跨 provider 有 0.227 中位距离，统计行为不"通用"）；
2. **任意端点=一个配置**：参考源与目标端点都是 `ProviderConfig{base_url, api_key, protocol}`，enroll/audit 走同一条采集管线，杜绝"参考通道与审计通道行为不一致"的系统性偏差；
3. **测量有效性先于测量结果**——论文发现 0.76% 响应被注入推理轨迹、端点可能忽略温度/禁用标志；无效测量必须被探测并丢弃，否则验证结论无意义；
4. **验证是统计判定而非司法判决**——输出"通过/可疑/不确定 + 证据明细"，不做绝对断言；
5. **成本是设计约束且运行期强制**——单次审计 ≤ 240 个单 token 查询（几分钱量级），超预算立即中止；
6. **CLI 必须快**——Go 实现，启动毫秒级，采集并发最大化吞吐。

---

## 2. 系统架构

```
                    ┌─────────────────────────────────────────────┐
                    │            onetoken (Go, 单二进制)            │
                    │  enroll / audit / calibrate / probe / report │
                    └──────────────┬──────────────────────────────┘
                                   │
        ┌──────────────┬───────────┼───────────────┬──────────────┐
        ▼              ▼           ▼               ▼              ▼
┌──────────────┐ ┌────────────┐ ┌────────────┐ ┌────────────┐ ┌──────────────┐
│ collector/   │ │preprocess/ │ │  verify/   │ │ calibrate/ │ │  reporter/   │
│ 并发采集器     │ │ 归一化+分类 │ │  验证器    │ │ 阈值校准   │ │ 报告与告警   │
│ (worker pool │ │            │ │ (JSD+τ)    │ │ (ROC/EER)  │ │ (html/template│
│  幂等可恢复)  │ │            │ │            │ │            │ │  自动转义)    │
└──────┬───────┘ └──────┬─────┘ └─────┬──────┘ └─────┬──────┘ └──────┬───────┘
       │                │             │              │              │
       ▼                ▼             ▼              ▼              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                     store/（JSON/JSONL 文件存储，原子写）                  │
│  responses / fingerprints / audits / calibrations / models / drift         │
└───────────────▲──────────────────────────────────────────────────────────┘
                │ 统一调用：所有参考源与目标端点都经此层
┌───────────────┴──────────────────────────────────────────────────────────┐
│                     provider/ 统一提供商调用层（§6 专章）                   │
│  ProviderConfig{name, base_url, api_key, protocol}                        │
├───────────────────────────────┬──────────────────────────────────────────┤
│ ① OpenAI Responses API        │ ② OpenAI chat/completions 兼容（广泛）      │
│    POST {base}/v1/responses   │    POST {base}/v1/chat/completions         │
│    （o 系/gpt-5+，含 reasoning │    （OpenRouter/OpenAI/智谱/DeepSeek/       │
│     tokens 显式统计）          │     vLLM/Ollama/Azure…）                    │
├───────────────────────────────┴──────────────────────────────────────────┤
│ ③ Anthropic messages          │  协议自动协商 + 显式指定 + 失败降级告警      │
│    POST {base}/v1/messages    │  统一 ResponseRecord（文本/usage/延迟/raw）  │
└──────────────────────────────────────────────────────────────────────────┘
```

### 2.1 模块职责（Go 包）

| 模块 | 职责 | 关键接口 |
|---|---|---|
| `internal/provider` | **统一调用层（§6）**：三协议适配、协议协商、单请求级重试（429/5xx 分类、Retry-After 解析、jitter）、逐响应元数据、响应字节/完成长度上限、成本护栏 | `type Provider interface { Complete(ctx, Req) (*ResponseRecord, error) }` |
| `internal/battery` | 40-cell 探针电池定义与提示词加载（与配置分离） | `Battery`（10 任务 × 4 语言，cell 清单） |
| `internal/collector` | 并发采集：worker pool（per-provider 并发上限 4–8）、幂等键、可恢复续采、种子打乱、限流预算、失败重试与总 deadline | `RunBattery(ctx, provider, cells, n, T) → []Raw` |
| `internal/preprocess` | **归一化 + 分类**（valid/invalid/refusal/empty），在采样与探测之前运行 | `NormalizeClassify(raw) → Processed` |
| `internal/detector` | 测量有效性探测与清洗（依赖 preprocess 分类结果），与 verify 解耦 | `Screen(processed) → {ok, flags, cleaned}` |
| `internal/fingerprint` | 分布估计、**基 2 JSD（自写，直接按论文 Eq.1）**、指纹对象（构建自 `store.Fingerprint`） | `Distance(a, b *Fingerprint) (float64, int)`——返回 (距离, 参与 cell 数)，参与数供上层按有效 cell < k_min 判 inconclusive；`Build(responses) (*Fingerprint, error)` |
| `internal/verify` | 判定：JSD vs τ（按 (k,n,通道) 匹配校准库），含 inconclusive 缓冲 | `Verify(ctx, target Provider, claimed Fingerprint, k, n) → Verdict` |
| `internal/calibrate` | genuine/impostor 试验（分裂半奇偶切分原语）、ROC/AUC/EER、bootstrap CI、(k,n,通道) 分档（M1 范围）；LOO 1-NN 已实现（复现用途，投产路径 v1.2）；UPGMA/ARI（报告，v1.1）为后续里程碑 | `Calibrate(genuine, impostor []float64, opts) → *store.Calibration`（分档键 Scope/K/NPerCell/通道 与 CalibratedAt 由调用方填充；空输入或非有限阈值返回 nil 无效校准）；`SplitHalves` / `LOO1NN` |
| `internal/reporter` | 距离矩阵、聚类图（v1.1）、单端点报告、告警；**Go `html/template` 默认转义防 XSS** | `Report(auditID) → md/html` |
| `internal/store` | JSON/JSONL 文件存储：目录布局（§4.1）、原子写（tmp+rename）、JSONL 追加、幂等去重索引、证据链（append-only + raw_sha256）、schema_version 校验 | SaveAudit / AppendResponse / LoadResponses / SaveFingerprint / ... |
| `internal/config` | 配置加载（YAML/环境变量），密钥用不可序列化类型；**所有阈值/规则集中配置** | `Load(path) → Config` |
| `cmd/onetoken` | CLI 入口（cobra）：enroll/audit/calibrate/probe/report/drift | `main.go` |

### 2.2 数据流（一次完整审计）

```
前置条件：claimed_model 已 enroll（models 表 + 参考指纹）。无参考指纹的模型走 §3.5 互比降级。

enroll 阶段（低频，如每周）：
  参考源 ProviderConfig → provider/（协议协商+请求） → collector/（并发电池调度）
    → preprocess/（归一化+分类） → detector/（清洗与 QC） → fingerprint/ → store/（版本化）

audit 阶段（高频，如每日）：
  目标端点 ProviderConfig + 声称模型 → detector/（测量有效性预检：3~5 个探针 cell）
    → collector/（k cell × n 次，默认 8×15=120 查询，幂等去重）
    → preprocess/ → detector/（清洗：无效/推理/缓存签名）
    → verify/（指纹距离 vs τ） → Verdict{pass|suspicious|inconclusive} → store/ + reporter/ 告警
```

**流水线顺序约束**（避免依赖环）：`normalize → classify → screen(detector) → dist/JSD/verdict(verify)`。preprocess 独立，verify 只做分布+距离+判定；responses JSONL 只追加不修改（取证语义），派生列（normalized/classification）由 preprocess 一次性写入。

---

## 3. 核心算法规范

### 3.1 探针电池（与论文一致，40 cell）

| 任务 | 答案空间 | 语言（英/俄/中/阿） |
|---|---|---|
| 随机数 1–100 | 闭（100） | ×4 |
| 随机数 1–10 | 闭（10） | ×4 |
| 最喜欢的数字 | 开（数值） | ×4 |
| 随机字母 | 闭（字母表） | ×4 |
| 随机词 | 开 | ×4 |
| 随机颜色 | 开（规范化） | ×4 |
| 最喜欢的颜色 | 开（规范化） | ×4 |
| 随机动物 | 开 | ×4 |
| 随机城市 | 开 | ×4 |
| 抛硬币 | 闭（2） | ×4 |

- 采集参数：`temperature=1.0`，输出上限 16 token（论文 12→16 的最终值，各协议按命名映射：chat 用 `max_tokens`、responses 用 `max_output_tokens`、Anthropic 必填 `max_tokens`），固定 system 提示强制一词回答，尽力禁用推理（chat 传顶层 `reasoning_effort` 最低档、Responses 传 `reasoning:{effort:"minimal"}`、Anthropic 传 `thinking:{type:"disabled"}`）——具体取值与支持性由 §6.2 能力探测确认，o 系若拒绝最低档则按论文排除；
- 每个 cell 采样 **n=30**（T=1.0）建立参考指纹 + **n=3**（T=0）确定性变体（论文配置）；**前沿定价模型（≥$5/1M input tokens，论文定义）**T=1.0 采样减为 15；
- 审计模式（轻量）：k ∈ {8, 16} 个随机 cell 子集 × n=15。

### 3.2 归一化与分类（与论文 §IV-B 一致）

Unicode NFC → 剥离标点/引号 → 大小写折叠 → 阿拉伯-印度/中文数字映射为拉丁数字 → 取首个 token → 逐语言颜色词表映射规范码。**词表规范（M1.6 对齐）**：颜色 canonical 码集以论文 22 码为准（red/blue/green/yellow/orange/purple/violet/pink/black/white/gray/brown/cyan/turquoise/magenta/indigo/teal/gold/silver/azure/crimson/emerald，来源论文 color-lexicon.json，含项目扩展键超集）；硬币 canonical 为 h/t（论文 COIN 表）。

输出分类：`valid / invalid / refusal / empty`，**无静默丢弃**；指纹只使用 `valid`。refusal/有效率**不进指纹**（论文：对安全层变化鲁棒）。

**归一化边界测试**：阿拉伯-印度数字（٠-٩）、中文数字（一二三 vs 十/百）、emoji、多 token 首 token 切分、全角/半角——用 Zenodo 黄金样本做属性测试。**注意**：Go 侧无 tokenizer，首 token 切分用 Unicode 空格/标点启发式（论文同为文本级处理，无需加载模型 tokenizer）。

### 3.3 距离与判定

- 指纹 `F(M) = {(t,ℓ): p̂_{t,ℓ}}`（有效样本的经验分布）+ T=0 变体；
- 距离（论文 Eq.1）：

  D(Mₐ, M_b) = (1/|B′|) Σ₍ₜ,ℓ₎∈B′ JSD(p̂ᵃₜ,ℓ ‖ p̂ᵇₜ,ℓ)，JSD 取基 2，cell 双方 ≥10 有效样本；

  **T=0 变体（v0.6 明确语义）**：T=0 每 cell 采样 n=3，无法达主距离 ≥10 门槛，其比较门槛放宽为双方 ≥1 有效样本（`DistanceT0`）；T0 距离仅作确定性比对辅助信号（§5 `temperature-not-honored` 的分布侧工具），**不进入判定主路径**，最终口径以 M2.4 探测器实现为准；

- **实现要点（Go 自写，直接按论文公式）**：JSD 基 2 = `(KL(p‖m) + KL(q‖m)) / (2·ln2)`，m=(p+q)/2；**KL 取自然对数、整体除以 ln2**；采用 **0·ln0=0 约定、无任何平滑**——JSD 中 m 的支撑是 p∪q 的并集，从不出现未定义项，加性平滑会系统性改变每个值，与论文"支持不相交也可用"的约定冲突；**单位标度（sqrt vs 原始）以 M1 前置门 pin 论文实现语义为准**（scipy `jensenshannon` 返回的是 sqrt(JSD)，若论文用 scipy 则其常数是 sqrt 标度，反之亦然）——"值域 [0,1]"不足以区分两种标度；**M1.5 前置门已完成（v0.9）：pin 定原始标度（无 sqrt）、基 2、无平滑，与既定实现一致（详见 `docs/OneToken-M1.5-语义pin记录.md`）；数据不可得时的空置兜底见 §9.1**；M1 验收必须含**距离值级回归测试**，按 §9.1 的分层重放方案执行；
- 判定：`s = D(ref, target)`，若 `s ≤ τ` → **pass**，否则 → **suspicious**；
- **操作点选择**：默认误报优先——τ 对应校准 ROC 上 FPR ≈ 1% 的点（而非 EER 点）。配套约束：① τ 是 (k, n, 通道) 的函数，校准按档存储、审计精确匹配；② 阈值邻域判定缓冲：|s−τ| 落在 bootstrap CI 内时判 **inconclusive**，触发重复审计；③ 校准输出同时给出 τ_fpr1 处的 TPR 估计与 CI（操作点必须定义漏报半边）；④ τ 的 1% 分位数在小样本下 CI 极大，报告 bootstrap CI，样本不足回退全局档并标注。

- **τ_fpr 计算语义（v0.7 明确）**：取 FPR ≤ target 的**最宽松**阈值（FPR 最大的满足点，误报不超限）；target 低于数据分辨率（最小非零 FPR 不可达，如小样本/同值样本导致 FPR 跳变）时阈值非有限（τ=−∞）——**该档位视为无效校准，Calibrate 返回 nil，调用方回退全局档并标注**（上述 ④ 样本不足回退口径）；AUC 用曼-惠特尼 U 统计（tie 取 0.5），与 sklearn `roc_auc_score` 对拍一致（黄金值已落库单测）；得分含非有限值时过滤后重算，全部被过滤视为空输入；

- **genuine/impostor 对构造归属**：`SplitHalves` 为奇偶切分原语；半指纹构建、模型配对与 D_genuine/D_impostor 聚合在 M1.6 重放 harness 完成（本里程碑只交付算法层与校准记录）；

- **τ 的 CI 缺口（v0.7 记录，M2.5 裁决）**：§3.3 ②"|s−τ| 落在 bootstrap CI 内判 inconclusive"需要 τ 的 CI，当前校准实现只产 TPR 的 CI（store.Calibration schema 亦无 τ CI 字段）；M2.5 落地时裁决（补 τ CI 字段或重新解释为 TPR CI 近似）；τ_fpr5 仅存阈值，其 TPR 由 M3.2 评估时从 ROC 重算；

### 3.4 阈值校准

- **genuine 试验**：同一模型参考指纹与目标端点的分裂半对（按重复奇偶切分）→ 分布 D_genuine；
- **impostor 试验**：模型 X 指纹 vs 模型 Y 指纹（Y≠X）→ 分布 D_impostor；
- 输出：ROC 曲线、AUC、EER、τ_fpr1 / τ_fpr5、各自 TPR 与 bootstrap CI；
- **分档维度**：(k_cells, n_per_cell, ref_channel, target_channel) × (global / family / size-tier)；审计时精确匹配，无匹配档时强制全电池校准或拒绝审计；
- ROC/AUC/EER/UPGMA/ARI/1-NN **Go 自写**（各几十行），须单测：perfect/random 分类器 AUC=1/0.5 的构造性校验。

### 3.5 谱系辅助信号（标注 v1.2，可选）

对无法建立参考指纹的模型（无第一方 API、无开源权重），退化为**端点间互比**：与已建档模型做最近邻（LOO 1-NN）分类，输出"最接近的已知模型家族"——如论文中 `palmyra-x5` 落入 Qwen 邻域的先例。此模式产出**线索**而非判定。

---

## 4. 数据模型（JSON/JSONL 文件存储）

**存储形态（用户评审决议，v0.5）**：分目录 JSON/JSONL，替代 SQLite。理由：单用户 CLI、数据按语义天然分片、导入导出与开放共享（论文精神）、避免单大文件的全量读写与并发陷阱。响应数据按审计分片为 JSONL 追加文件（O(1) 写入），不随数据量增长而全量重写。

### 4.1 目录布局

```text
data/                              # 根目录可配置（默认 ~/.onetoken/data/，ONETOKEN_DATA 覆盖）
├── models.json                    # 模型目录（全量，小）
├── calibrations.json              # 校准结果，按 (scope,k,n,通道) 分档（全量，小）
├── drift.json                     # 漂移趋势（全量，小）
├── fingerprints/
│   └── <model_id>.json            # 每模型一文件（含版本链 superseded_by）
├── audits/
│   └── <audit_id>.json            # 每次审计一文件（结果+元数据）
└── responses/
    └── <audit_id>.jsonl           # 本次审计响应，JSONL 追加（只增不改）
```

### 4.2 一致性约定（替代原 DB 约束）

| 原 DB 能力 | JSON 替代实现 |
|---|---|
| 外键/CHECK 约束 | 应用层校验 + schema_version 校验；model/audit 引用为**弱关联**（审计/指纹文件保留快照字段，不强引用，便于导出分享） |
| 幂等去重（部分唯一索引） | 响应按 `cell+sample_idx` 内存 map 去重：重跑审计时读回本次 `responses/<id>.jsonl` 建索引 |
| 事务/原子性 | **原子写**：所有整文件写入走 临时文件 + `os.Rename`（同目录，POSIX 原子）；JSONL 用 `O_APPEND` 单行追加 |
| 证据链（raw 只增不改） | JSONL **只追加不修改**；每条响应含 `raw_sha256` 可校验；审计/指纹文件整写，内含快照字段 |
| 索引查询 | 不需要——数据按语义分片（按 audit_id / model_id 取文件） |
| 并发 | 原子写 + 可选 `flock`（不同 audit 写不同文件，冲突面极小；单进程 CLI 场景足够） |

时间戳统一 `YYYY-MM-DDTHH:MM:SSZ`（强制 Z 后缀保证字典序）。**所有整文件 JSON 顶层含 `schema_version`，读取时校验（不匹配拒绝加载）**；JSONL 行（responses）为追加日志，豁免版本字段——行格式由 `raw_sha256` 与行结构约束（版本演进走字段兼容）。

### 4.3 文件结构

**models.json**：`{ "schema_version": 1, "models": [ {id, vendor, family, model_type, ref_source, catalog_snapshot, notes} ] }`

**fingerprints/<model_id>.json**：

```json
{
  "schema_version": 1,
  "model_id": "qwen/qwen3-8b",
  "version": "2026-07-11v1",
  "collected_at": "2026-08-05T00:00:00Z",
  "ref_source": "local",
  "cells": { "random_number_100:en": { "dist": {"42": 12, "57": 6}, "n": 30, "t": 1.0, "valid_rate": 0.99 } },
  "t0_cells": { "random_number_100:en": { "dist": {"42": 3}, "n": 3, "t": 0.0 } },
  "qc_flags": [],
  "superseded_by": ""
}
```

（dist 为原始计数，归一化在 fingerprint 计算层完成；每 cell 的 n 记录实际有效样本数）

**audits/<audit_id>.json**：

```json
{
  "schema_version": 1,
  "id": "<audit_id>",
  "endpoint": "base_url + model string",
  "claimed_model": "openai/gpt-5.1",
  "ref_fingerprint_version": "2026-07-11v1",
  "k": 8, "n": 15,
  "selected_cells": ["random_number_100:en"],
  "seed": 12345,
  "score": 0.18,
  "threshold": 0.15, "threshold_scope": "global",
  "verdict": "pass",
  "cells_detail": { "random_number_100:en": 0.12 },
  "qc_flags": [],
  "audited_at": "2026-08-05T00:00:00Z"
}
```

（预检阶段先建 pending 状态的审计文件，再采集写响应，最后更新 verdict；verdict ∈ pending|pass|suspicious|inconclusive|error）

**responses/<audit_id>.jsonl**（每行一个响应对象，只追加）：

```json
{ "cell": "random_number_100:en", "sample_idx": 3, "temperature": 1.0,
  "prompt_hash": "...", "raw_completion": "42", "raw_sha256": "...",
  "normalized": "42", "classification": "valid",
  "reasoning_tokens": 0, "finish_reason": "stop",
  "latency_ms": 812, "provider": "upstream-x", "reported_model": "gpt-5.1",
  "usage": {}, "cost_usd": 0.0001, "ts": "2026-08-05T00:00:00Z" }
```

**calibrations.json**：`{ "schema_version": 1, "calibrations": [ {scope, k, n, ref_channel, target_channel, genuine_n, impostor_n, auc, eer, tau_fpr1, tau_fpr1_tpr, tpr_ci, tau_fpr5, roc, calibrated_at} ] }`

**drift.json**：`{ "schema_version": 1, "entries": [ {model_id, ref_fingerprint_version, audit_id, scores, flag, updated_at} ] }`

**导入导出**：按语义分文件的 JSON 直接拷贝/分享即导出（与论文数据开放精神一致）；响应 JSONL 可与 CSV 互转；全库导出 = 打包 data/ 目录。

---

## 5. 测量有效性探测器（detector/）

论文工程化的关键附加层——**任何验证结论都依赖测量条件真实有效**。探测器依赖 preprocess 的分类结果：

| 探测项 | 方法 | 判定标准与依据 |
|---|---|---|
| **推理痕迹** | 读取协议层透传的 `reasoning_tokens`、`finish_reason` 与 usage 明细 | **确定性证据（无需启发式）**：`reasoning_tokens > 0`（Responses 的 `output_tokens_details` 与 chat 的 `completion_tokens_details` 均显式提供；Anthropic 侧 `thinking` 未禁用成功时 usage 出现思考 token 类别）；`finish_reason == "length"` 即截断异常。**退化到论文启发式**（仅当协议层无法提供上述字段时）：completion token 数显著 > 一词答案（正常 1–6，异常端点 40–60）或可见推理轨迹（论文：0.76% 响应、14 个模型-provider 组合）→ 统一标记 `hidden-reasoning`（两种机制，论文以同一理由排除） |
| **T=0 一致性** | 对 3~5 个 cell 发 T=0 查询，与参考 T=0 答案比对 | 论文观测：provider 内确定性 cell 占比 90.4%，跨 provider 降至 84.5%（是"确定性 cell 占比"非"一致率"）；参考侧 T=0 每 cell 仅 3 样本，3/3 全中概率 0.729——探针 **n≥5**，阈值按二项检验换算并经本地实测校准；不一致 → `temperature-not-honored` |
| **响应级缓存签名** | T=1.0 响应方差 + 延迟分布联合筛查 | 命中即**完整性违规嫌疑**并按异常处理（不进指纹、单独告警）。**注意**：论文普查 14/2040 cell 命中均归因 provider 负载波动（良性），"命中≠异常"，本签名精度未知，需按目标生态重新校准 |
| **有效率/拒答** | preprocess 分类统计 | **cell 级门槛采用论文规则**：双方 ≥10 有效样本才进入 JSD 平均（论文 Eq.1）；`valid 率 < 80%` 仅作**模型级 QC 指标**（论文试点 per-model 标准，非按 cell 弃用）；refusal 率突变 → `safety-layer-change` |
| **端点可达性** | 重试统计 | 持续失败 → `unreachable`（论文：5+1 个端点损耗先例）；审计侧有效 cell 数 < k_min 时判 **inconclusive** |

被标记 `hidden-reasoning` / `response-caching` / `temperature-not-honored` 的测量**不进入指纹与判定**，且自身即告警项。

---

## 6. 统一提供商调用层（专章）

> 用户评审意见 1：对任意 BaseURL 的给定 API Key 进行请求，包含 Responses / OpenAI chat completions 兼容 / Anthropic 三种接口；参考注册与待审核模型都通过此层。

### 6.1 配置模型

**base_url 语义（关键约定）**：`base_url` 为协议根（**不含** `/v1` 路径段），provider 层统一拼接 `/v1/responses`、`/v1/chat/completions`、`/v1/messages`。auto 协商可对含/不含 `/v1` 两种形态做兜底探测，但主约定如上，配置示例即按此书写。

```yaml
# config/providers.yaml —— 任意端点 = 一个配置，enroll 与 audit 同构
providers:
  - name: openrouter
    base_url: https://openrouter.ai        # 语义：不含协议路径段，层内统一拼 /v1/<endpoint>
    api_key_env: OPENROUTER_API_KEY      # 密钥一律走环境变量，不落盘
    protocol: auto                       # auto | responses | chat | anthropic
    limits: { rpm: 100, rpd: 5000, max_concurrency: 8, timeout_s: 60 }
    headers:                             # 附加头（如 OpenRouter 的 X-Title）
      HTTP-Referer: "https://example.com"
  - name: local-qwen
    base_url: http://localhost:8000        # 本地参考通道（vLLM）
    api_key_env: LOCAL_API_KEY
    protocol: chat
    limits: { rpm: 0, rpd: 0, max_concurrency: 16, timeout_s: 120 }   # 本地不限流；并发可高于默认 4–8
  - name: anthropic
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
    protocol: anthropic
    limits: { rpm: 50, rpd: 2000, max_concurrency: 4, timeout_s: 60 }
```

### 6.2 协议适配矩阵

| 维度 | ① Responses (`/v1/responses`) | ② Chat (`/v1/chat/completions`) | ③ Anthropic (`/v1/messages`) |
|---|---|---|---|
| 认证 | `Authorization: Bearer` | `Authorization: Bearer` | `x-api-key` + `anthropic-version` |
| 请求体 | `{model, input, instructions, temperature, max_output_tokens, reasoning:{effort:"minimal"}, store:false}` | `{model, messages, temperature, max_tokens, reasoning_effort:"minimal"}`（**顶层字符串**，非嵌套对象；o 系取值 low/medium/high，gpt-5 家族 minimal/low/medium/high，按能力探测降级） | `{model, messages, max_tokens, temperature, thinking:{type:"disabled"}}` |
| 输出提取 | `output[]` 中过滤 `type=="message"` 且 content `type=="output_text"` 的项（数组可能含 reasoning/工具 item） | `choices[0].message.content` | `content[].text` |
| 推理禁用 | `reasoning:{effort:"minimal"|"low"}`（"none" 非文档化取值且 o 系实测拒绝，见 §1.2 候选实验路径）+ **`usage.output_tokens_details.reasoning_tokens` 显式统计** | 顶层 `reasoning_effort`（部分端点忽略，按能力探测降级）+ **`usage.completion_tokens_details.reasoning_tokens` 显式统计** | `thinking:{type:"disabled"}`（extended thinking 已 GA，**无需 beta 头**；仅历史端点可选） |
| usage 归一 | `input/output`；cached 属 `input_tokens_details.cached_tokens`，reasoning 属 `output_tokens_details.reasoning_tokens` | `prompt/completion(+cached)`；reasoning 属 `completion_tokens_details` | `input/output/cache_creation/cache_read` |
| 限流响应 | 429 + Retry-After（秒） | 429 + Retry-After（秒） | 429 + `retry-after-ms`（**毫秒**） |
| 特殊 | 无 `messages`（用 `input`+`instructions`）；新模型用 `max_output_tokens` | 新模型（o 系/gpt-5）用 `max_completion_tokens`；**Azure OpenAI 形态特殊（`/openai/deployments/{d}/chat/completions?api-version=`，含 deployments 段），与任意 BaseURL 模式不兼容，需独立 adapter，MVP 不承诺（v1.1）** | `max_tokens` 必填；temperature 与 top_p 不同设 |

### 6.3 协议协商（`protocol: auto`）

1. **显式指定优先**：`protocol` 非 auto 时直接锁定，失败不自动换协议（避免误用导致密钥错配）；
2. **auto 推断**：按 base_url 特征（`api.openai.com`→responses 优先、`api.anthropic.com`→anthropic）初判；
3. **能力探测**：发送一个最小探测请求（1 个 cell、T=0、1 样本），按响应分类：200→锁定；401/403→密钥错误（**不降级**）；**404/405→协议不匹配，尝试下一协议**；**400→请求体/参数问题（如 effort 取值、参数形态错误），记日志并中止**（换协议会掩盖真实 bug）；连续三种失败→`protocol-undetermined` 告警并中止；
4. **每次会话锁定协议**，写入 `responses.usage_json` 的 `protocol` 字段，审计可比性要求同协议比对优先。

### 6.4 统一 ResponseRecord（三协议收敛为同一结构）

```go
type ResponseRecord struct {
    Protocol         string            // responses | chat | anthropic
    Text             string            // 提取后的回答文本
    RawCompletion    string            // 原样保留（证据链 + raw_hash）
    FinishReason     string            // stop | length | ...（length 即截断/异常确定性信号，支撑成本护栏）
    ReasoningTokens  int               // responses 的 output_tokens_details / chat 的 completion_tokens_details 显式提供；其他协议 0
    CompletionTokens int
    PromptTokens     int
    CachedTokens     int               // prompt-cache 命中数（缓存签名探测用）
    LatencyMS        int64
    Provider         string            // 上游路由（OpenRouter 等透传）
    ReportedModel    string
    CostUSD          float64
    TS               time.Time         // UTC
}
```

**安全性（语言无关，Go 实现时强制）**：禁用重定向（`http.Client{CheckRedirect: 返回 http.ErrUseLastResponse}`，显式配置，§10）；scheme 校验（https，localhost 例外）；**SSRF 拦截范围**：RFC1918（10/8、172.16/12、192.168/16）、环回 127/8、链路本地 169.254/16、CGNAT 100.64/10、IPv6 ::1 与 fc00::/7，可配置白名单；**DNS rebinding 防护**：DialContext 中对解析出的每个 IP 先校验再拨号（解析→校验→连接），仅校验配置字符串可被解析侧绕过；base_url 与密钥绑定校验（防止把 A 厂商密钥误配到恶意 base_url）；日志/异常脱敏（Authorization 头永不落日志）。

---

## 7. 参考指纹双通道设计

### 7.1 统一抽象（Go 接口）

```go
// 接口契约（设计层伪代码，非实现）
type ReferenceSource interface {
    Channel() string            // "local" | "official-api"
    Provider() *provider.Provider // 经统一调用层的 Provider 包装
    ModelRef() ModelRef
}
```

**通道的实质是"ProviderConfig + 权重溯源"，而非独立采集代码**——双通道通过统一调用层复用同一套采集/归一化/探测器管线，杜绝实现分叉。

### 7.2 通道 A：开源模型本地部署（LocalHost）

- **部署形态：优先 vLLM**（原生支持 `temperature=1.0`，默认 top_p=1.0/top_k=-1 纯 softmax 采样，与论文协议一致）；显式传 `top_p=1.0, top_k=-1, repetition_penalty=1.0` 防全局默认被改，T=0 探针可传 `seed` 复现；
- **Ollama 受限使用**：llama.cpp 采样管线默认 `top_k=40/top_p=0.9/repeat_penalty=1.1` 会**截断长尾**（低概率尾部恰是论文身份信号所在：cell 中位熵仅 1 bit）且惩罚提示词重叠词——污染 T=1.0 分布，而 T=0 探测器**无法发现**。仅当显式传 `options:{temperature:1.0, top_p:1.0, top_k:-1, repeat_penalty:1.0, min_p:0}` 且经**同权重形状校验**（vLLM vs Ollama 各采 100+ 样本，JSD < 0.05）才允许启用；
- **权重一致性保证**：记录模型文件哈希/版本 tag，与目标端点声明的 checkpoint 对齐（如 `qwen3-235b-a22b-2507` 的 2507 快照）；
- **成本**：一次性部署成本；单模型指纹采集 1200+ 查询本地无成本。

### 7.3 通道 B：厂商官方 API（OfficialAPI）

- **实现**：OpenAI / Anthropic / 智谱 / DeepSeek / 百度 等第一方 API，**一律经统一调用层**（§6），协议按厂商自动/显式协商；
- **约束**：闭源模型（GPT/Claude 系列）只能走此通道；**无第一方 API 的聚合器独家模型无法建立参考指纹**（→ §3.5 降级）；
- **成本**：每模型指纹采集按论文普查量级 ≈ $0.21/模型平均（1320 查询 ≈ $0.14）；前沿定价模型**工程外推** $1–5（非论文数据，待实测修正）；
- **风险**：官方 API 本身 serving 栈有分布差异（论文：gpt-4 跨 Azure vs OpenAI 达 0.392）——参考指纹**标注来源通道**，验证优先同通道比对；跨通道比对用校准后的 τ（论文跨 provider AUC 0.880 而非 0.971）。

### 7.4 指纹来源决策表

| 声称模型类型 | 参考指纹来源 | 备注 |
|---|---|---|
| 开源（Qwen/Llama/GLM/DeepSeek…） | 本地部署（推荐）或官方 API | 本地部署最省钱且权重可控 |
| 闭源有官方 API（GPT/Claude/文心…） | 官方 API | 唯一可靠来源 |
| 闭源无官方 API（聚合器独家） | 无 → 降级为端点间互比 | 只能给"最接近家族"线索 |

### 7.5 双通道语义（明确裁决）

- **能力层双通道**：LocalHost 与 OfficialAPI 均受支持；
- **每模型默认单源**：按 §7.4 决策表选一个来源（避免成本翻倍；闭源模型客观上无第二源）；
- **可选双源交叉校验**：开源模型可同时建档本地+官方两枚指纹（分别标注版本），跨通道距离作为服务栈差异基线（论文：跨 provider 中位 0.227），用于解释跨通道审计结果，不进默认判定路径。

---

## 8. 操作模式与 CLI（Go 单二进制）

```bash
# 建档：为模型建立参考指纹（经统一调用层，参考源可以是任意 ProviderConfig）
onetoken enroll --provider local-qwen --model qwen/qwen3-8b
onetoken enroll --provider openai-official --model openai/gpt-5.1

# 探测：测量有效性预检
onetoken probe --provider openrouter --model openai/gpt-5.1

# 审计：验证端点真伪（前置条件：claimed_model 已 enroll）
onetoken audit --provider openrouter --claimed-model openai/gpt-5.1 \
               --k 8 --n 15 [--tau auto] [--json]
#   --tau auto：按 (k,n,通道) 从校准库精确匹配；无匹配档则拒绝审计或强制全电池校准

# 校准 / 报告 / 漂移
onetoken calibrate --scope global --recompute
onetoken report --audit <id>        # 单端点报告（逐 cell JSD 明细）
onetoken report --matrix            # 距离矩阵热力图
onetoken report --tree              # UPGMA 聚类图（v1.1，复刻论文 Fig.2）
onetoken drift --model qwen/qwen3-8b

# 任意端点直传（临时/一次性场景，无需先写 providers.yaml）：
#   --base-url <url> --api-key-env <ENV> [--protocol auto|responses|chat|anthropic] [--headers k=v,...]
#   密钥仍走环境变量引用，不落盘、不进日志。

# 性能特征（口径说明）：
# --help 与配置加载 <50ms（冷启动：含 data 目录初始化）；
# 120 查询审计典型 3–20s（网络主导：8 并发下 120÷8=15 波 × 单请求 0.3–1.5s；<2s 仅本地 vLLM 可达；不含重试/退避与 T=0 预检）
```

**性能设计（用户评审意见 2）**：
- Go 单二进制，零外部运行时；启动/解析 <50ms；
- 并发采集：per-provider worker pool（默认 4–8，配置可覆盖，本地通道可更高），每个 cell 的 n 次重复并发执行；总吞吐受 provider 限流与网络延迟主导，CPU/内存占用可忽略；
- 采集进度实时输出到 stderr（进度条），stdout 只输出结构化结果（JSON/Markdown），便于管道集成；
- 可复现性：审计随机种子持久化（`selected_cells_json`），同种子重跑结果一致。

---

## 9. 校准与验收实验设计

### 9.1 里程碑 M1：复现论文指标（验证实现正确性）

- 输入：论文 Zenodo 开放数据集（**固定版本 + 校验和**；数据集 doi:10.5281/zenodo.21278557、软件归档 doi:10.5281/zenodo.21278793，避免标签映射漂移）；
- 动作：重放归一化 → 分布 → JSD 矩阵 → 验证 ROC；
- **前置门（先做，pin 论文实现语义）**：用论文软件归档核对论文实际计算方式——是否 scipy、base 参数、是否取 sqrt、是否平滑——**先确认常数标度，再定回归目标**；✅ **已完成（2026-08-05/06，M1.5）**：JSD 基 2、原始标度（无 sqrt）、0·ln0=0 无平滑、MIN_N=10、R pROC 做 ROC/EER——与 Go 实现一致，结论与差异清单详见 `docs/OneToken-M1.5-语义pin记录.md`；
- 验收（**指标分层，按统计层次分别重放**）：
  - **距离值级回归（必须）**：两个层次分开——**cell 级中位数**：同模型分裂半 0.075、跨模型 0.489（6,564 genuine / 107 万 impostor cell 对）；**模型对级 D 中位数**：噪声底线 0.140、跨 provider 0.227——各 ±5% 容差；**M1.6 实测（v0.10 更正）**：0.075/0.489/0.140 精确命中；**0.227 由 L8 复现**（数据集同一 slug 多 provider 记录——75/165 included 模型有多 provider，如 gpt-4o=OpenAI+Azure、llama-3.3-70b 达 11 provider；按 (model,provider) 拆指纹，56 对同模型跨 provider 距离中位 **0.2230**，±5% 内命中；另实测全部 impostor 模型对中位 0.483）；另固定若干**逐 cell 黄金样本对**（来自 Zenodo 数据）做精确值校验；
  - 全矩阵结构检查：重放出的同模型 genuine 对距离整体 < impostor 对距离，中位数贴近上述常数；
  - 论文级指标（单调不变量，不能单独作验收）：AUC ≈ 0.971、EER ≈ 7.3%、家族分类 ≈ 59.5%（±2pp）；**ARI 低值（论文 0.023）属正常**，勿当 bug 排查；
  - 内部自洽兜底：同模型两半低 JSD、跨模型高 JSD；算法单测：ROC/AUC 用 perfect/random 构造性校验（AUC=1/0.5）、归一化黄金样本测试；
  - M2 隐含前置：三协议 **URL 构造单测**（base_url 拼 `/v1/<endpoint>`，防双 /v1）。
- **数据不可得兜底（v0.8，保留备用；截至 2026-08-05/06 数据已获取、兜底未触发）**：① **用户决策（已确认）**：Zenodo 数据集/软件归档不可获取时，**M1.5 前置门空置**、不阻塞后续，改由用户自备权威数据（官方账号/API 采集真实响应）替代；② **验收口径（用户已确认 2026-08-05）**：上述论文数值项（cell 级 0.075/0.489、模型对级 0.227/0.140、AUC 0.971 / EER 7.3% / 家族分类 59.5% / ARI 0.023）标注 **N/A（数据不可得）**；验收**降级**为“自备数据回归基线记录 + 全矩阵结构检查（genuine < impostor）+ 内部自洽兜底”；JSD 常数标度维持 M1.3 既定实现（§3.3）并标注“语义未 pin”，校准记录与回归报告需带该标注；⚠️ **循环验证注意**：自备数据经同一管线采集处理，无法独立验证管线本身（系统性归一化 bug 会让结构检查照常通过）；Zenodo 数据若日后可得，需回 pin 复核标度并重跑 M1.6。

### 9.2 里程碑 M2：双通道冒烟

- 试点：本地部署 **Qwen3-8B** 建档（≥2 个开源模型）；**统一调用层三协议各跑通一次 enroll**（local-chat / openai-responses / anthropic），验证协议协商与 ResponseRecord 收敛；
- 对 OpenRouter 同名端点 audit，**验收标准为"判定与跨通道校准后 τ 一致"，如实报告实际距离**——不预设"必须 pass"：论文实测 29% 同模型跨 provider 对超出 impostor 区间、OpenRouter 多上游路由可能造成审计不稳定，健康端点也可能 fail（生态事实而非实现 bug）；审计响应记录上游 provider 字段用于解释不稳定；
- 用**不同模型**端点冒充 → 期望 suspicious（impostor）；
- 验收：探测器 flag 正确性断言（reasoning_tokens、T=0 探针、缓存签名、有效率、hidden-reasoning）。

### 9.3 里程碑 M3：替换模拟实验

- **样本量**：真/假目标各 ≥8（检出率 90% 时 3 个样本全中概率仅 0.729）；每端点重复审计 ≥5 次取判定序列，报告二项 CI；
- **假目标构造（明确定义）**：(i) 本地可控——vLLM 加载 AWQ/GPTQ 量化权重、旧版 checkpoint；(ii) 公网——不同模型端点。本地-本地比对无服务栈噪声、结果偏乐观，**与公网结果分开报告**；
- **验收（先定指标后定数值，主辅双操作点）**：
  - 主评估点 τ_fpr1：TPR ≥ T（T 由 M1 校准数据预先确定并记录，预计 60–80% 区间，如实报告 CI）；
  - 辅评估点 τ_fpr5：TPR ≥ 90%；
  - 同端点 5 次重复审计 verdict 一致性 ≥ 80%。

### 9.4 里程碑 M4：持续运行

- 每日调度审计（cron 显式 UTC，刷新窗口与审计窗口错峰）+ 告警 + 参考指纹月度刷新 + 漂移趋势报告；
- **验收（区间验收，非"零误报"）**：10 端点 × 每日 × 14 天 ≈ 140 次审计，误报数落在 τ_fpr1 对应二项 CI（≈0–4 次）内；**人为注入的替换全部被捕获**（注入协议需明确定义：注入什么、多频繁、谁判定捕获）。

---

## 10. 技术栈选型（Go）

| 层 | 选型 | 理由 / 注意 |
|---|---|---|
| 语言 | Go 1.22+ | 启动毫秒级、单二进制、goroutine 并发适配批量采集；IO 密集场景下性能与 Rust 同级，开发/迭代快（用户拍板） |
| HTTP | 标准库 `net/http`：自定义 `Transport`（连接池、超时、DialContext）+ `http.Client` | 无重框架；**禁用重定向需显式配置**（默认是跟随最多 10 次）：`CheckRedirect: func(...) error { return http.ErrUseLastResponse }`，位置在 `http.Client` 而非 `Transport` |
| 存储 | 标准库 `encoding/json` + 文件系统（分目录 JSON/JSONL，§4） | 原子写 tmp+rename；JSONL 追加 O(1)；无第三方依赖；schema_version 兼容校验；导入导出天然 |
| CLI | `spf13/cobra` | 子命令体系（enroll/audit/…）；`--help` 毫秒级 |
| 配置 | YAML + 环境变量（密钥） | 所有阈值/规则进配置，不硬编码；密钥类型禁止序列化进日志/报告 |
| 日志 | 标准库 `log/slog`（结构化） | 脱敏：Authorization 头、密钥字段永不输出 |
| 统计 | `gonum.org/v1/gonum`（stat）；ROC/AUC/EER/UPGMA/ARI/1-NN/bootstrap **自写** | 各几十行；须构造性单测（AUC=1/0.5 等） |
| 报告 | `html/template`（**默认上下文感知转义**，天然防 XSS）+ 自写 SVG/ASCII 热力图 | 恶意端点注入 HTML/JS 无效；**`template.HTML`/`template.JS` 类型会绕过转义**——模型输出只进文本节点，SVG 结构由内部常量生成 |
| 交叉编译 | `GOOS=windows|linux|darwin go build` | 单二进制分发（Windows 侧也可直接跑） |

### 10.1 安全基线（语言无关，Go 强制）

1. **密钥管理**：`api_key_env` 环境变量引用，**永不落盘/落日志/落报告**；配置错误（A 厂商密钥配到 B base_url）→ 启动时绑定校验失败告警；
2. **输出安全**：`html/template` 默认转义所有模型输出与 JSON 字段（存储型 XSS 防护）；raw 内容默认折叠展示；CSV/JSON 导出与 HTML 分开；
3. **成本护栏（运行期强制）**：响应字节上限、completion 长度上限（>阈值即标记 hidden-reasoning 并中止该 cell）、单次审计总 token/总成本预算，超预算立即中止并记 `inconclusive`；
4. **限流与滥用防护**：per-provider RPM/RPD 预算、并发上限 4–8（默认，配置可覆盖）、429 尊重 Retry-After（含 Anthropic 毫秒变体）、指数退避 + jitter、**突发铺开**（与普通流量交错，呼应 T2 缓解与论文"速率限制亏空均匀扩散"策略）；4xx 校验错误不重试、5xx 指数退避、最大重试次数与单次审计总 deadline；
5. **SSRF 防护**：禁用重定向（`CheckRedirect` 显式返回 `ErrUseLastResponse`）、scheme 校验（https，localhost 例外）、内网/链路本地地址拦截（RFC1918/环回/链路本地/CGNAT/IPv6 私有段，可配置白名单）、DNS rebinding 防护（DialContext 解析→校验→拨号）。

---

## 11. 成本估算

| 场景 | 量级 | 估算 |
|---|---|---|
| 建档（参考指纹/模型） | 40 cell × 30 + 3×40 = 1320 查询（**理论最小查询数**） | 本地部署≈$0；官方 API ≈ $0.14（理论最小）– $0.21（论文实测均值，含重试与损耗；**预算按 $0.21 取**）/ $1–5（前沿，**工程外推，待实测**） |
| 单次审计 | 8×15=120 或 16×15=240 查询 | 普通模型几分钱；前沿几美分（论文：hundredths of a cent 到 a few cents） |
| 持续审计（10 端点 × 每日） | ~2400 查询/日 | 月成本 < $10（普通模型池） |
| 校准（一次性/按需） | 复用建档数据，新增少量对 | ≈ $0.5–5 |

---

## 12. 漂移与生命周期管理

1. **参考指纹 TTL**：默认 30 天，到期自动刷新并保留历史版本（`superseded_by` 链）；
2. **漂移监测**：同一模型连续审计得分趋势上升（近 5 次均值 > 参考噪声底线 0.140 且递增）→ 标记 `stale`，触发刷新；**时间窗口一律 UTC**；
3. **模型更新联动**：`-latest` 滚动别名 → 每次审计同时比对最近 N 个指纹版本，输出"更像哪个版本"；
4. **跨通道一致性**：本地与官方参考若同时存在（§7.5 可选双源），定期交叉校验——跨 provider 有 0.227 中位距离，用容忍阈值而非零容忍。

---

## 13. 威胁模型与对抗考虑

| 对手 | 系统应对 | 残余风险 |
|---|---|---|
| T1 静默替换 | 完整防御（核心场景） | — |
| T2 识别审计提示并路由 | 提示改写家族 + 审计时随机采样 + 与普通流量交错（限流铺开） | 专用对抗研究留待论文式实验；工程上可配置改写模板 |
| T3 仿冒分布 | 崩溃为忠实性问题（匹配任意日常提示的分布≈运行该模型） | 高保真蒸馏理论上可逼近；超出 MVP 承诺 |
| **测量层面**（忽略温度/禁用标志、注入推理、响应缓存） | §5 探测器显式拦截；Responses API `reasoning_tokens` 提供确定性证据 | 新机制出现时需扩展探测器（插件化） |
| **指纹漂移**（参考过期） | §12 TTL + 趋势监测 | 更新频繁的模型在刷新窗口内可能误报 |

---

## 14. 里程碑与交付物

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| **M1** | store + 归一化 + JSD + 校准模块（Go）；论文数据复现（**先复刻论文实现语义**） | 前置门 pin 语义；分层值级回归通过（cell 级 0.075/0.489、模型对级 0.227/0.140，±5%）**且** AUC 0.971 / EER 7.3% ±2pp；算法单测就绪；**数据不可得时按 §9.1 兜底口径验收**（前置门空置、论文数值项 N/A、标注“语义未 pin”） |
| **M2** | 统一调用层三协议 + 并发采集器 + 探测器 + 双通道参考 + 端到端审计 | 三协议各跑通一次 enroll（含 **URL 构造单测**，防双 /v1）；判定与校准后 τ 一致；探测器 flag 正确；≥2 开源模型 |
| **M3** | 替换模拟实验 + 主辅操作点 + 报告 | τ_fpr1 处 TPR≥T（先定指标）；τ_fpr5 处 TPR≥90%；真/假目标各 ≥8；重复审计一致性 ≥80% |
| **M4** | 调度 + 告警 + 漂移管理 + CLI 完整 | 140 次审计误报 ∈ 二项 CI（≈0–4）；注入替换全部捕获 |

**目录结构（Go 惯例）**

```
onetoken/
├── go.mod
├── cmd/onetoken/main.go        # CLI 入口（cobra）
├── internal/
│   ├── config/                 # YAML + 环境变量；阈值/规则集中
│   ├── provider/               # 统一调用层：协议协商 + 三协议适配 + 重试/限流/护栏
│   ├── battery/                # 40-cell 探针电池 + 提示词
│   ├── collector/              # 并发采集（worker pool、幂等、续采）
│   ├── preprocess/             # 归一化 + 分类
│   ├── detector/               # 测量有效性探测
│   ├── fingerprint/            # 分布、基 2 JSD
│   ├── verify/                 # 判定（含 inconclusive 缓冲）
│   ├── calibrate/              # ROC/AUC/EER/bootstrap/1-NN/UPGMA/ARI
│   ├── store/                  # JSON/JSONL 文件存储（原子写/幂等/证据链）
│   └── report/                 # 报告（html/template 转义）
├── config/
│   ├── prompts.json            # 40 cell 提示词（模板与配置分离）
│   └── providers.yaml.example  # 无密钥模板
├── tests/                      # Go 单测 + 集成测试（M1 黄金样本）
└── docs/
```

---

## 15. 部署与运维（OneToken 自身）

1. **构建与分发**：`go build ./cmd/onetoken` 产出单二进制；交叉编译 `GOOS=windows|linux|darwin`（纯标准库依赖，全平台可编译）；发布到 PATH 即可用；
2. **配置清单**：`config/providers.yaml`（无密钥模板）+ 环境变量注入密钥 + `config/prompts.json`；提示词模板与配置分离，生成时插值校验防注入；
3. **调度示例**（cron，UTC）：

   ```cron
   0 6 * * *   onetoken audit --provider openrouter --claimed-model <...> --k 8 --n 15
   0 3 1 * *   onetoken enroll --refresh-all
   0 4 1 * *   onetoken calibrate --scope global --recompute
   ```

4. **数据落盘**：`data/` 目录可配置（默认 `~/.onetoken/data/`，`ONETOKEN_DATA` 覆盖）；**备份**：每日打包 `data/` 目录 + 校验和（证据链用途）；**恢复**：解包即恢复——无迁移脚本，仅校验各文件 `schema_version` 兼容；
5. **日志与审计追踪**：结构化日志（secret 脱敏），审计运行日志可追溯；报告文件按敏感级标注（含端点 URL、延迟、成本，共享时注意泄露运营信息）；
6. **升级路径**：数据格式变更走 `schema_version` 兼容校验（读旧写新，文件级迁移）；CLI → REST 风格 HTTP 服务（v2，鉴权；Go 侧 `net/http`）。

---

## 16. 风险与局限（诚实清单）

1. **参考指纹获取是硬约束**：无第一方 API、无开源权重的模型只能做互比线索，无法严格验证；
2. **错误率本质**：单次审计不是 100% 判定；误报优先操作点提高漏报率，需人工复核与重复审计；τ 小样本 CI 如实报告；
3. **指纹漂移**：模型更新频繁的端点存在验证窗口错位风险；
4. **闭源模型参考仅反映"官方 API 行为"**：官方 API 与聚合器端点即使权重相同也有分布差异（0.227 中位），阈值需跨通道校准；OpenRouter 多上游路由可能造成单端点审计不稳定；
5. **合规边界**：仅使用付费配额内的普通请求；不做攻击性探测；结果作为统计偏差报告而非指控（与论文伦理章节一致）；
6. **单聚合器结论不泛化**：论文普查基于 OpenRouter；异常基线需按目标生态重新校准；
7. **本地参考通道实现风险**：Ollama 采样参数污染 T=1.0 分布（§7.2）；本地权重版本与远端声明不一致导致误报——enroll 记录权重哈希并展示；
8. **测量有效性探测器精度**：缓存签名"命中≠异常"（论文 14 例均良性）、T=0 小样本误标风险——阈值经本地校准并定期复审；
9. **协议差异风险**：三协议 behavior 差异（temperature 语义、usage 口径、限流单位）统一收敛在 provider 层；auto 协商失败的端点会中止而非静默用错协议——避免"测了个寂寞"；
10. **能力探测与 o 系风险**：`reasoning.effort` 取值与支持因模型而异（"none" 非文档化取值、o 系实测拒绝）——指纹化 o 系前必须实测 `reasoning_tokens=0`，否则按论文排除；Azure OpenAI 形态特殊，MVP 不承诺（v1.1 独立 adapter）。

---

*本文档依据论文 arXiv:2607.10252 的协议与数据设计；v0.10 并入 M1.6 重放结果（七层对拍全命中：0.075/0.489/0.140/0.483、AUC 0.971318、EER 0.0730、1-NN 59.5%；**0.227 由 L8 复现命中 0.2230**——数据集同一 slug 多 provider 记录，同模型跨 provider 距离；preprocess 颜色/硬币词表对齐论文 canonical），M1.5 语义 pin 结果（JSD 原始标度/基 2/无平滑与既定实现一致、R pROC 工具链、采样参数确认，§3.3/§9.1/头部决策记录）；v0.8 并入用户决策（M1.5 数据兜底：Zenodo 不可得时前置门空置、改由用户自备权威数据替代、JSD 标度维持既定实现并标注未 pin，§3.3/§9.1，含审查后归因拆分与 §14 兜底分支）；v0.7 并入 M1.4 校准算法实现后的接口契约演进（§2.1 calibrate 签名、§3.4 τ_fpr 计算语义）；v0.6 并入 M1.3 实现后的接口契约演进（§2.1 fingerprint 签名、§3.3 T=0 变体语义）；v0.5 并入用户评审意见（统一提供商调用层、Go 性能选型、JSON/JSONL 存储）与五轮对抗式审查结论。*
