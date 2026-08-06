# M1.5 前置门：语义 pin 记录

> **任务**：M1.5（实施计划 §2）。核对论文软件归档的实现语义，确定 JSD 常数标度等复现口径。
> **结论一句话**：**论文 JSD 为基 2、原始标度（不取 sqrt）、0·ln0=0 无平滑——与我们的 Go 实现（M1.3）完全一致，无需任何改动**。
> **日期**：2026-08-05/06（中国时区）｜**依据版本**：Zenodo 软件归档 doi:10.5281/zenodo.21278793（`pamela-publish-code.zip`，md5 `d81de3b8ef5c0bca74fd7c2bdbb41a6b`，已验证）、数据集 doi:10.5281/zenodo.21278557（`pamela-publish-data.zip`，md5 `f2ce3fba3081f73e9908179fb2f061b6`，已验证）

---

## 1. 下载与校验和

| 对象 | DOI | 文件 | 状态 | 校验和 |
|---|---|---|---|---|
| 软件归档 | 10.5281/zenodo.21278793 | `pamela-publish-code.zip` | ✅ 已下载（本机→服务器中转） | md5 `d81de3b8ef5c0bca74fd7c2bdbb41a6b` |
| 数据集 | 10.5281/zenodo.21278557 | `pamela-publish-data.zip`（52,366,366 B） | ✅ **已下载完成**（2026-08-06；续传守护方案，Zenodo 支持 Range 但 Cloudflare 间歇限流，浏览器 UA+Accept 头缓解；跳板服务器侧被限流 3B/s 不可用） | md5 `f2ce3fba3081f73e9908179fb2f061b6`（**与 Zenodo 声明一致**，已验证） |

**网络事实**：本机 zenodo.org 可达（HTTP 200，首字节 ~10s）；跳板服务器 TCP 可连但文件流被限流（单线程 3B/s、aria2c 8 线程失败）；本机直连 13–100KB/s 波动（浏览器 UA + Accept 头缓解 Cloudflare 限流；**Zenodo 真实文件响应支持 Range 续传**，403 错误页不支持）；**终版方案：续传守护脚本（`-C -` + HTML 错误页检测 + `--retry-all-errors` 无限重试，挂 systemd 用户服务），一次完成**。软件归档 62KB 经跳板服务器下载成功回传。

## 2. 语义 pin 结论（逐项核对，证据=软件归档文件）

### 2.1 JSD 距离（`stats/03-divergence.js`）——✅ 与 M1.3 一致

```javascript
const log2 = Math.log2;
function jsd(p, q) {
  const support = new Set([...Object.keys(p), ...Object.keys(q)]);
  let d = 0;
  for (const x of support) {
    const px = p[x] ?? 0, qx = q[x] ?? 0, mx = (px + qx) / 2;
    if (px > 0) d += 0.5 * px * log2(px / mx);
    if (qx > 0) d += 0.5 * qx * log2(qx / mx);
  }
  return d;
}
```

| pin 点 | 论文实现 | 我们实现 | 结论 |
|---|---|---|---|
| base | `Math.log2` | 基 2 | ✅ 一致 |
| **取 sqrt** | **否**（函数直接返回 d，注释 "bounded [0,1]"） | 原始标度 | ✅ **常数标度 pin 定：原始，无 sqrt** |
| 平滑 | `p[x] ?? 0` + `if (px > 0)`，0·ln0=0 | 无平滑 | ✅ 一致 |
| 公式 | `Σ 0.5·px·log2(px/mx) + 0.5·qx·log2(qx/mx)`，m=(p+q)/2 | 同（KL(p‖m)+KL(q‖m))/(2·ln2) 等价） | ✅ 一致 |
| 支撑 | 并集（含仅一方出现的符号，另一方按 0） | 同 | ✅ 一致 |
| **舍入** | `+v.toFixed(4)` 四舍五入到 4 位小数——**同时作用于 per-cell JSD、mean 矩阵与 split-half score 三处** | 不舍入 | ⚠️ 复现对拍需模拟（差异 < 5e-5/cell） |

> scipy 对照：`scipy.stats.jensenshannon(p,q,base=2)` 返回 `sqrt(JSD)`，**平方后**与论文手工实现一致——M1.3 已按平方后对拍（8 组，容差 1e-12），结论不变。

### 2.2 距离聚合（`03-divergence.js`）——✅ 与 M1.3/Eq.1 一致

- cell 级过滤：双方 `n_valid >= MIN_N`（**MIN_N = 10**）才进入平均——对应设计 Eq.1；
- 仅 Study A cell（`paper === 1` 的 10 任务 × 4 语言 = 40 cell）+ `included` 模型集合；
- 模型对级 D = 参与 cell 的 **算术平均**；
- 全矩阵：`a >= b` 时跳过（对称矩阵只算一半）。

### 2.3 split-half 与 genuine/impostor 构造（`03-divergence.js`）——✅ 与 M1.4 一致

- 半指纹：按 **rep 奇偶**切分（`rep % 2 === 0` → half A，否则 half B）；只取 `temperature === 1 && answer_class === 'valid'`；
- 半门槛：`n >= MIN_N / 2`（=5）；
- trial：ref 的 A 半 vs probe 的 B 半；`genuine = (ref === probe)`；
- **模型对级聚合**：`aggregate(jsd ~ ref + probe, FUN = mean)` 后进入 ROC（对应设计"模型对级 D 中位数 0.227 / 噪声底线 0.140"层次）；
- 验证用固定种子 `20260704`。

### 2.4 ROC/AUC/EER（`stats/R/12-verification-roc.R`）——⚠️ 工具链差异

- **R pROC 包**（非 scipy）；`direction = ">"`，`levels = c(FALSE, TRUE)`（genuine=cases）；JSD 低 = genuine；
- AUC 语义 = 曼-惠特尼 U（pROC 与 sklearn/scipy 同源）——M1.4 与 sklearn 对拍结论不变；
- **EER = (FPR+FNR)/2 在 |FPR−FNR| 最小点**——与 M1.4 一致 ✅；
- budget 曲线：k ∈ {1,2,4,8,12,16,24,32,full}，M=200 次重采样，5%/95% 分位 CI——M2.4/M3.2 参考口径。

### 2.5 1-NN 家族分类（`stats/R/11-classification.R` + `lib.R:model_families`）——✅ 语义一致，⚠️ 复现隐患

- LOO 在 **mean JSD 矩阵**上取最近邻（`which.min`）；准确率 = 正确率均值；**仅 ≥2 成员的家族参与评估**——与 M1.4 LOO1NN 一致 ✅；
- family 标签推导链：`family`（人工策展，可覆盖）→ `family_guess`（config 内模型的回退）→ slug 前缀（不在 config 的模型）——**注意：发布数据 342 条全无 `family` 字段（0/342），实际全部走 `family_guess`**（21 个不同值，含 "other"）；
- ⚠️ **复现隐患**：`model_families()` 对缺失 `family` 列的 `setNames(..., sel$id)` 会抛 "names attribute must be same length" 错误（静态推演）——M1.6 重放 `11-classification.R` 可能直接崩溃，需前置验证或改用 Go 侧自写 LOO（M1.4 已实现）。

### 2.6 归一化管线（`stats/01-normalize.js`）——✅ 主路径一致，2 处差异记录

| 步骤 | 论文实现 | 我们实现 | 结论 |
|---|---|---|---|
| NFC | `raw.normalize('NFC')` | NFC（x/text/norm） | ✅ |
| 标点剥离 | `[«»"“”„'’‘`().,!?。！？、：:;؛؟\[\]{}*_#-]+` → 空格 | 同思路 | ✅ |
| 大小写 | `toLowerCase()` | 同 | ✅ |
| 数字 | 阿印/波印 `[٠-٩۰-۹]` → 拉丁 | 同 + 全角 | ✅ 超集 |
| **中文数词** | **0–99**（正则 `([零一二两三四五六七八九])?十?([零一二两三四五六七八九])?`：零→0、十→10、十七→17、四十二→42；无百/千/万/亿） | 支持到亿 | ⚠️ 我们是超集；论文数据答案范围 ≤100（num100-random 1-100），zh 模型若答 "一百"（100）论文判 invalid、我们判 100 valid——M1.6 对拍需验证实际影响 |
| integer 任务 | 提取首个 `-?\d+`，`answer_space` 范围检查 | 同 | ✅ |
| binary 硬币 | 首词查表：en h/t；ru орёл/орел/решка；zh 正面/正/反面/反；ar صورة/كتابة | 同表（含 head 补丁） | ✅ |
| word 任务 | 首词小写；`>3` 词判 off-format（invalid） | 多词前置判定 | ✅ |
| letter(grapheme) | 任意词中**第一个单字符 token**（`words.find(单字符)`）；zh 语言跳过此分支 | random_letter 放宽单字母 | ⚠️ 描述精确化 |
| **refusal 词表** | 固定：`/(i can.?t\|i cannot\|i'm sorry\|as an ai\|не могу\|извин\|抱歉\|无法\|لا أستطيع\|عذراً\|آسف)/i`（**大小写不敏感**） | 四语言**超集**（含"不知道""не знаю"等，审查扩充） | ⚠️ 复现对拍可能有效率差异，需记录 |
| 颜色词表 | `color-lexicon.json`（22 canonical 码） | 同结构 | ✅ |
| **无静默丢弃** | answer_class ∈ {valid, invalid, refusal, empty, **post_reasoning**}，全部保留 | 同 | ✅ |
| **post_reasoning** | `reasoning_len > 0`（或 model@provider 推理对推断：样本 n≥20 且推理率 ≥0.3）→ 排除指纹 | M2.4 hidden-reasoning 探测器 | ✅ 语义对应 |

### 2.7 分布构建（`stats/02-distributions.js`）——⚠️ 舍入差异

- 概率 = 计数/总有效数；**`toFixed(4)` 4 位小数舍入**（复现需模拟）；熵在**未舍入概率**上计算；
- 熵 = `-Σ p·log2(p)`（基 2）✅；`validity_rate`/`entropy_bits`/`mode_share` 为 `toFixed(3)`；
- `validity_rate = n_valid / (n_valid + off)`（off=invalid+refusal+empty+post_reasoning）✅；
- T=0：`deterministic = 唯一答案数 ≤ 1`；**分 provider 判定**（`deterministic_within_provider`）——与设计 §5 一致 ✅。

### 2.8 采样参数（`config/run.config.json`）——✅ 与设计 P0.3 默认值一致

| 参数 | 论文 | 设计文档 | 结论 |
|---|---|---|---|
| T=1.0 | main 30 / pilot 20 reps | n=30 | ✅ |
| T=0 | 3 reps | n=3 | ✅ |
| 前沿模型 | `expensive_input_threshold_usd_per_mtok = 5.0`，reps×0.5 | 前沿（≥$5/1M 输入）n=15 | ✅ |
| max_tokens | **16**（12 被 OpenAI 拒，文档注明 "valid answers are 1-6 tokens"） | 输出上限 16 token | ✅ |
| 语言 | en/ru/zh/ar | 四语言 | ✅ |
| 并发/重试 | 16 并发；重试 5、退避 2–60s；超时 90s | 配置可覆盖 | ✅ |
| 推理 | `reasoning:{enabled:false}` | 协议层关闭 | ✅ |

### 2.9 数据集规模（`config/models.selected.json`）

全量 342 模型条目、165 `included`；`family_guess` 去重：全量 21、included 165 去重 **19**（≥2 成员 **17**）。**M1.6 复现时以 `included:true` + Study A 40 cell 为准**（与 `03-divergence.js` 的过滤逻辑一致）。

---

## 3. 对我们的实现的影响评估

1. **JSD/fingerprint（M1.3）**：语义一致，**零改动**。常数标度悬案已 pin：原始标度（无 sqrt）。
2. **calibrate（M1.4）**：EER 语义、LOO 1-NN 语义一致，零改动。ROC 方向需在 M1.6 对拍时用真实数据验证（pROC `direction=">"` 的实际效果以复现结果为准）。
3. **复现对拍时的差异处理清单**（M1.6）：
   - 分布 4 位小数舍入（`toFixed(4)`）；
   - per-cell JSD 4 位小数舍入；
   - refusal 词表差异（我们是超集）→ 若有效率出现系统性偏差，用论文词表复跑归一化比对；
   - 中文数词范围（无实际影响，论文答案 ≤100）。
4. **归一化分类**：论文的 `post_reasoning` 类在归一化阶段排除，我们对应在 M2.4 探测器阶段排除——语义一致，管道位置不同（已记录）。

## 4. 论文黄金值验证（来自数据集 `results/` 产物）

| 指标 | 数据集产物（论文官方值） | 设计文档声称 | 结论 |
|---|---|---|---|
| AUC | `verification.json` full_battery.auc = **0.971342** | ≈ 0.971 | ✅ |
| EER | full_battery.eer = **0.07282** | ≈ 7.3% | ✅ |
| 试验量 | 165 genuine / 27,060 impostor（模型对级） | 165 included 模型 | ✅ |
| 1-NN 家族分类 | `classification.json` accuracy = **59.5%**（163 模型，chance 18.4%） | 59.5% ±2pp | ✅ |
| budget 曲线 | k=1 EER 23.3%、k=8 10.65%、k=16 9.45%（M=200，5%/95% CI） | — | 参考 |
| 分层数据 | `split-scores.json`（205MB）、`divergence.json`（35MB）、`divergence-matrix.csv`、`distributions.json`、`normalized.jsonl`（95MB）、`clustering.json`（UPGMA/ARI） | — | **M1.6 对拍黄金值** |

> 数据与代码已归档至 `data/zenodo/`（gitignore 排除）；M1.6 重放 harness 可直接对拍 `results/` 产物与我们的 Go 管线输出。

## 5. 遗留项

- [x] **preprocess 词表已对齐（M1.6）**：硬币 canonical h/t（原 heads/tails）、颜色词表替换为论文 22 canonical（106 键 + 31 扩展 = 137 键），inClosedSpace 同步，单测更新；L7 实测硬币一致率 0.02→0.98、颜色 canonical 0.31→0.92；
- [ ] M1.6 重放 harness 按 §3 差异清单实现（分布/JSD 4 位舍入、refusal 词表超集、中文数词 0-99 vs 超集）——✅ 已完成（cmd/replay，L1–L7 七层对拍全命中，详见实施计划 M1.6 行）；
- [ ] 复现 `11-classification.R` 的 R 侧崩溃隐患（`setNames` 缺失列）——M1.6 用 Go 侧 LOO（59.509% vs 论文 59.5% 命中）✅；
- [ ] （可选）`stats/R/10-clustering.R`（UPGMA/ARI，报告 v1.1 用）语义核对留待 M3.3 前。

## 6. M1.6 补充事实（2026-08-06）

- **颜色任务口径差异（记录在案）**：论文颜色任务 `normalized`=原词首 token、canonical 放 `color_canon` 副字段（分布构建用 normalized=原词，跨语言分离）；我们的 preprocess `normalized`=canonical（跨语言合并，设计增强）——M2 自采数据时以我们口径校准，与论文数值对拍时以论文数据为输入（算法层已验证一致）。
- **L7 归一化层一致率**：8 个非颜色任务 class 一致率 0.96–0.99（残余差异=refusal 词表超集、多词判定 >1 vs >3 等已知差异）；硬币 canonical 0.9804；颜色 canonical 0.92（残余=扩展键 beige/navy 非论文 canonical 等）。
- **0.227（M1.6 已复现）**：设计文档原记为“论文正文独立实验值（同模型跨通道）”；M1.6 审查发现**数据集同一 slug 多 provider 记录**（75/165 included 模型有多 provider：openai/gpt-4o-2024-05-13=OpenAI+Azure、meta-llama/llama-3.3-70b-instruct 达 11 provider），L8 按 (model,provider) 拆分布、同 model 不同 provider 对（56 对）cell 级 mean 距离**中位 0.2230，±5% 命中论文 0.227**（抽查 gpt-4o Azure vs OpenAI=0.2034、gemini flash-lite Google vs AI Studio=0.1027）；全部 impostor 模型对（27060）中位 0.483（论文同口径 0.4832）。
