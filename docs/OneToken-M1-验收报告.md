# OneToken M1 里程碑验收报告

> **任务**：M1.7（实施计划 §2）。汇总全部单测与回归报告，逐条对照设计文档 §9.1/§14 验收项。
> **结论**：**M1 验收通过**（11 项通过 + 1 项移交 M2.1 + 1 项不适用，无未过项）。
> **日期**：2026-08-06｜提交：`6c696b4`（M1.5+M1.6）；M1.7 文档变更待提交

---

## 1. 交付物清单

| 交付物 | 位置 | 说明 |
|---|---|---|
| 存储层（JSON/JSONL） | `internal/store` | M1.1：原子写/幂等/证据链/schema_version，15 单测 |
| 归一化+分类 | `internal/preprocess` | M1.2：NFC/数字映射/四语言 refusal/颜色·硬币词表（对齐论文），30 单测 |
| 指纹/距离 | `internal/fingerprint` | M1.3：KL/JSD 基 2 原始标度/Build/Distance，16 单测（scipy 黄金对拍） |
| 校准 | `internal/calibrate` | M1.4：ROC/AUC/EER/bootstrap CI/LOO1NN，16 单测（sklearn 对拍） |
| 配置 | `internal/config` | P0.3：密钥脱敏/绑定校验/采样参数，13 单测 |
| 电池 | `internal/battery` | P0.2：40 cell 提示词加载与防注入，8 单测 |
| 语义 pin 记录 | `docs/OneToken-M1.5-语义pin记录.md` | M1.5：论文实现语义核对 + 差异清单 |
| 重放 harness | `cmd/replay` | M1.6：八层对拍（L1 分布/L2 JSD/L3 ROC/L4 cell 级/L5 模型对级/L6 1-NN/L7 归一化/L8 跨 provider） |
| 论文数据 | `data/zenodo/`（gitignore） | 数据集 md5 `f2ce3fba3081f73e9908179fb2f061b6`、软件归档 md5 `d81de3b8ef5c0bca74fd7c2bdbb41a6b`（均与 Zenodo 声明一致） |

**单测**：98 个测试函数，`go test ./...` 全绿（含 `-race` 通过记录）；构建/vet/gofmt 干净。

## 2. §9.1 验收清单（逐项）

| # | 验收项（设计 §9.1） | 证据 | 状态 |
|---|---|---|---|
| 1 | 输入：Zenodo 数据**固定版本 + 校验和** | 双 zip md5 与 Zenodo 声明逐字节一致 | ✅ |
| 2 | **前置门 pin 论文实现语义**（scipy? base? sqrt? 平滑? cell 过滤） | M1.5：JS 手写（非 scipy）、基 2、**原始标度（无 sqrt）**、0·ln0=0 无平滑、MIN_N=10；与 Go 实现一致 | ✅ |
| 3 | **cell 级中位数**：0.075 / 0.489（6,564 genuine / 107 万 impostor cell 对）±5% | L4：**0.075 / 0.489 精确命中**；6,564 / 1,072,798 对与论文 split-scores 逐一对齐 | ✅ |
| 4 | **模型对级 D 中位数**：噪声底线 0.140、跨 provider 0.227 ±5% | L5 genuine 中位 **0.140 精确**；L8 同模型跨 provider 56 对中位 **0.2230**（±5% 内命中 0.227）；全部 impostor 模型对 0.483（论文同口径 0.4832） | ✅ |
| 5 | **逐 cell 黄金样本对精确值校验** | L2：536,649 对，最大差 0.0001（= toFixed(4) 半 ULP 边界，预期内） | ✅ |
| 6 | **全矩阵结构检查**：genuine 整体 < impostor，中位数贴近常数 | genuine 0.075（cell）/0.140（模型对）< impostor 0.489/0.483 | ✅ |
| 7 | 论文级指标 ±2pp：AUC ≈ 0.971、EER ≈ 7.3%、家族分类 ≈ 59.5% | L3：AUC **0.971318**（论文 0.971342）、EER **0.0729**（论文 0.07282）；L6：1-NN **59.509%**（论文 59.5%） | ✅ |
| 8 | ARI 低值（论文 0.023）属正常，勿当 bug | 数据集 `clustering.json` 备查（UPGMA/ARI 报告在 v1.1） | ✅ 备查 |
| 9 | **内部自洽兜底**：同模型两半低 JSD、跨模型高 JSD | L4/L5 分层均满足（0.075/0.140 vs 0.489/0.483） | ✅ |
| 10 | **算法单测**：ROC/AUC perfect/random 构造性（AUC=1/0.5）、归一化黄金样本 | M1.4 构造性测试、M1.2 黄金样本（含 40-cell 冒烟） | ✅ |
| 11 | M2 隐含前置：三协议 **URL 构造单测**（base_url 拼 `/v1/<endpoint>`，防双 /v1） | **属 M2.1 范围**，移交 M2 里程碑执行 | ⏭️ M2.1 |
| 12 | 数据不可得兜底（v0.8 用户决策） | 数据已获取，兜底未触发；策略保留备用 | ✅ 不适用 |

## 3. 回归数值汇总（M1.6 八层）

| 指标 | 我们 | 论文/设计 | 容差 | 结论 |
|---|---|---|---|---|
| 分布（L1） | 6,572 cell 逐值差 0 | — | — | 精确 |
| JSD 矩阵（L2） | 最大差 0.0001 | — | 4 位舍入边界 | 精确 |
| cell 级 genuine 中位 | 0.075 | 0.075 | ±5% | ✅ |
| cell 级 impostor 中位 | 0.489 | 0.489 | ±5% | ✅ |
| 模型对级噪声底线 | 0.140 | 0.140 | ±5% | ✅ |
| 模型对级跨 provider | 0.2230（L8, 56 对） | 0.227 | ±5% | ✅ |
| 模型对级全部 impostor | 0.483 | 0.4832 | — | ✅ |
| AUC | 0.971318 | 0.971342 | ±2pp | ✅ |
| EER | 0.0729 | 0.07282 | ±2pp | ✅ |
| 1-NN 家族分类 | 59.509% | 59.5% | ±2pp | ✅ |
| 结构检查 | impostor > genuine | 要求 | — | ✅ |

**复现说明**：`go run ./cmd/replay`（数据 `data/zenodo/dataset-raw`）；jsd 键排序保证确定性；cell 级 JSD 模拟论文 toFixed(4)；pair 均值不舍入（与论文 R 一致）。

## 4. 已知差异与下一迭代

**无未过验收项。** 以下为记录在案的差异/移交项（不阻塞 M1）：

| 项 | 说明 | 去向 |
|---|---|---|
| 归一化 refusal 词表超集 | 我们四语言词表含"不知道""не знаю"等（论文固定词表）——L7 实测 8 任务 class 一致率 0.96–0.99 | 文档记录；M2 自采数据以我们口径校准 |
| 中文数词超集 | 我们支持到亿（论文 0–99）；论文数据答案 ≤100 无实际影响 | 文档记录 |
| 颜色任务口径 | 论文 normalized=原词/canonical 放 color_canon；我们 normalized=canonical（跨语言合并，设计增强）——L7 canonical 一致率 0.92 | 文档记录；M2 自用口径校准 |
| **post_reasoning/hidden-reasoning 管道位置** | 论文在归一化阶段排除 `post_reasoning`；我们在 **M2.4 探测器阶段**排除——M2 自采数据在探测器未生效时（provider 静默忽略 `reasoning:{enabled:false}` 且不回报 `reasoning_tokens`）存在指纹污染风险 | **M2.4 必须生效**（o 系 gate：实测 `reasoning_tokens=0` 才可指纹化）；M1.5 §2.6/§3.4 记录 |
| 颜色扩展键 | beige/navy 等非论文 22 canonical（项目扩展） | 文档记录 |
| M2 隐含前置 URL 单测 | 三协议 base_url 拼 `/v1/<endpoint>` 构造单测 | **M2.1** |
| UPGMA/ARI 报告 | 聚类/ARI（论文 Fig.2 复刻） | **M3.3（v1.1）** |
| τ 的 CI 缺口 | calibrate 无 τ CI 字段（设计 §3.4 已记录） | **M2.5 裁决** |
| Zenodo 数据重下触发 | 下载含 Cloudflare 限流（续传守护方案），若需重下见 M1.5 记录 | 备查 |
