# M2.7 CLI 与端到端冒烟记录

> **日期**：2026-08-06 ｜ **状态**：mock 端到端通过；真实云端试点待执行（需用户配置密钥）
> **依据**：实施计划 M2.7、设计 §8（操作模式与 CLI）、§9.2（云端通道冒烟）

## 1. 交付内容

- **CLI 三命令**（cobra）：`enroll`（建档参考指纹）/ `probe`（测量有效性预检）/ `audit`（审计判定）
- **直传参数**：`--base-url --api-key-env --protocol [--headers]`（密钥只走环境变量；敏感头直传拒绝）
- **audit**：`--tau auto`（查校准库，无档拒绝审计）/ `--tau <float>`（直传阈值，冒烟/临时）；种子持久化（Audit.Seed/SelectedCells）
- **上游 provider 透传**：`X-Openrouter-Via` 等响应头 → `ResponseRecord.Provider` → Audit/输出（§9.2 解释不稳定）
- **启动性能**：`--help <50ms`（进程内实测 ~0ms，含 data 目录初始化的正式基准归 M4.3 ④）

## 2. Mock 端到端验证（测试内，无真实密钥）

`cmd/onetoken/e2e_test.go` 用 httptest 模拟三协议端点，全链路验证：

| 用例 | 结果 |
|---|---|
| enroll（直传参数）→ 指纹落盘（RefSource=official-api） | ✅ |
| audit 同模型端点（--tau 直传 0.2）→ **pass**，score≈0.006–0.013 | ✅ |
| impostor（不同模型池冒充）→ **suspicious**，score≈1 | ✅ |
| 三协议各 enroll 一次（chat / responses / anthropic） | ✅ |
| audit 无校准库（--tau auto）→ **ErrNoCalibration 拒绝**（设计 §3.4） | ✅ |
| 上游 provider 透传（mock 设 X-Openrouter-Via）→ 输出 upstream 字段 | ✅ |
| probe 健康端点 → flags 空 | ✅ |
| 未 enroll 的 audit → 报错提示先 enroll | ✅ |
| `--help` 启动 <50ms | ✅ |

**实际距离示例**（同模型）：score=0.013（采样噪声，远低于 τ=0.2）；impostor score≈1.0（不相交分布 JSD）。

## 3. 真实云端试点指引（待用户执行）

试点需真实密钥（环境变量），按设计 §9.2：

```bash
# 1. 建档（≥2 开源模型，云端 API；如智谱 glm-4.5 / 阿里 qwen）
export ZHIPU_API_KEY=...; export DASHSCOPE_API_KEY=...
onetoken enroll --provider zhipu --model zhipu/glm-4.5 --version 2026-08-06v1
onetoken enroll --provider dashscope --model qwen/qwen3-8b --version 2026-08-06v1

# 2. 审计 OpenRouter 同名端点（判定与校准后 τ 一致、如实报告距离、不预设 pass）
export OPENROUTER_API_KEY=...
onetoken audit --provider openrouter --claimed-model qwen/qwen3-8b --k 8 --n 15

# 3. impostor 冒充（不同模型）→ 期望 suspicious
onetoken audit --provider openrouter --claimed-model zhipu/glm-4.5 --k 8 --n 15
```

- 校准库为空时 audit 报 `ErrNoCalibration`——试点期可用 `--tau` 直传（如 0.20，跨 provider 基线 0.2230 量级）或先跑 calibrate（M4.3）。
- 如实报告：OpenRouter 多上游路由可能致健康端点 fail（论文实测 29% 同模型跨 provider 超出 impostor 区间），属生态事实非实现 bug；审计记录 upstream 字段用于解释。
- 三协议真实试点：responses（OpenAI/OpenRouter 新端点）/ anthropic（Anthropic）/ chat（其余）各 enroll 一次。

## 4. 已知限制（留痕）

- `safety-layer-change` 需参考 refusal 基线（指纹未持久化基线，下迭代接线；当前 probe/audit 不触发该项）
- `--tau` 直传为设计外扩展（冒烟/临时），决策已记入实施计划 §7
- SIGINT 无优雅中止（响应逐条落盘数据安全；M4.1 接入 signal）
- 探测器 flag 与有效率随 audit 落盘（audits/<id>.json QCFlags/CellsDetail）
