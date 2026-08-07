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

> **参考通道裁决（用户 2026-08-06，补正）**：参考端点**由用户自定，工具不作规定**——可选用厂商官方 API，也可选用户信任的任何云端端点（含聚合器）；下方示例用厂商官方 API 仅为示意。

试点需真实密钥（环境变量），按设计 §9.2：

```bash
# 1. 建档（≥2 开源模型；参考端点由用户自定——示例用厂商官方 API，可另选信任端点）
export DASHSCOPE_API_KEY=...; export ZHIPU_API_KEY=...
onetoken enroll --provider dashscope --model qwen/qwen3-8b --version 2026-08-06v1
onetoken enroll --provider zhipu --model zhipu/glm-4.5 --version 2026-08-06v1

# 2. 审计同名端点（判定与校准后 τ 一致、如实报告距离、不预设 pass；示例用 OpenRouter）
export OPENROUTER_API_KEY=...
onetoken audit --provider openrouter --claimed-model qwen/qwen3-8b --k 8 --n 15

# 3. impostor 冒充（不同模型）→ 期望 suspicious
onetoken audit --provider openrouter --claimed-model zhipu/glm-4.5 --k 8 --n 15
```

- 校准库为空时 audit 报 `ErrNoCalibration`——试点期可用 `--tau` 直传（如 0.20，跨 provider 基线 0.2230 量级）或先跑 calibrate（M4.3）。
- 如实报告：OpenRouter 多上游路由可能致健康端点 fail（论文实测 29% 同模型跨 provider 超出 impostor 区间），属生态事实非实现 bug；审计记录 upstream 字段用于解释。
- 三协议真实试点（enroll 端点由用户自定）：responses（如 OpenAI 官方或支持 responses 的端点）/ anthropic（Anthropic 官方）/ chat（智谱/DeepSeek 等 chat 端点）各一次。

## 4. 已知限制（留痕）

- `safety-layer-change` 需参考 refusal 基线（指纹未持久化基线，下迭代接线；当前 probe/audit 不触发该项）
- `--tau` 直传为设计外扩展（冒烟/临时），决策已记入实施计划 §7
- SIGINT 无优雅中止（响应逐条落盘数据安全；M4.1 接入 signal）
- 探测器 flag 与有效率随 audit 落盘（audits/<id>.json QCFlags/CellsDetail）

## 5. 真实云端试点结果（2026-08-06，DeepSeek）

**结论：DeepSeek 当前 API 两个模型均为推理模型，不可指纹化——探测器在真实端点正确排除（论文方法论验证）。**

| 项目 | 结果 |
|---|---|
| 端点 | `https://api.deepseek.com`（chat 协议，base_url 不含 /v1 ✓） |
| 可用模型 | `deepseek-v4-flash` / `deepseek-v4-pro` |
| 建档尝试（v4-flash） | ❌ **hidden-reasoning 拒绝**（1320 查询完成，探测器拦截） |
| 响应明细 | `reasoning_tokens=16`（输出全占）、`finish_reason=length`、`content=''`（无实际回答） |
| 请求参数 | 已传 `reasoning_effort: minimal`——DeepSeek 忽略（v4 为深度思考模型，无法关闭推理） |
| 判定 | 符合设计 §1.2/§5：`reasoning_tokens>0` 或 `finish=length` 即确定性证据 → 隐藏推理端点按论文排除，不可指纹化 |

**意义**：
1. 探测器 5 类 flag 的 `hidden-reasoning` 在真实端点**首次验证**——正确识别推理模型并拒绝建档（参考源不可信）；
2. 单 token 指纹法不适用于推理模型（输出被推理占满，无直接采样分布）——这是论文明确的适用边界，非实现缺陷；
3. 试点需**非推理模型端点**：如智谱 glm-4.5 系列、阿里 qwen 系列（非 thinking 版）、OpenAI gpt-4o-mini/gpt-5-mini 等（用户自选参考端点）。

## 6. 推理通道试点（系统 2，v0.19，DeepSeek）

**结论：推理通道（post-reasoning 回答指纹）端到端可行，但判别力弱于非推理通道，τ 需单独校准。**

| 项目 | 结果 |
|---|---|
| 建档 deepseek-v4-flash / v4-pro（--reasoning，max_tokens=512） | ✅ 两模型均成功（Channel=reasoning，40 cell 全有效） |
| 指纹质量 | ✅ dist 键为简短回答（h/t/blue/东京…）；**Text 管线修复**：归一化输入为提取的回答文本，非 RawCompletion（后者含响应唯一 id 会污染分布键——真实端点暴露、mock 掩盖） |
| 同端点 audit（claimed=flash，端点=flash，--reasoning） | ✅ **pass**，score=0.084（思考后回答的同模型噪声） |
| 跨模型 audit（claimed=flash，端点=pro） | k=8 抽样 0.1506（<τ=0.20 漏报）；**40 cell 全量 0.2376**（17/40 cell >0.2） |
| 判别力对比 | 推理通道 genuine 0.084 / impostor 0.238 vs 非推理 0.075 / 0.489——思考后回答更同质化（偏好信号被思考校正），间距窄 |
| 探测器适配 | hidden-reasoning 分流（不排除）；temperature-not-honored 对推理通道跳过（思考链 T=0 下仍随机） |
| 硬币偏好信号 | 推理模型思考后仍 h 偏好（28/30）——论文信号的推理通道形态 |

**推理通道 τ 建议**：单独校准（genuine ~0.08 / impostor ~0.24 量级），τ 取 0.15 附近；k=8 抽样波动大（impostor 0.15 漂移），审计建议 k=16 或全 cell 提升稳定性。M2.9 正式校准待 ≥2 推理模型的 genuine/impostor 全量数据。
