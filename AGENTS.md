# AGENTS.md — OneToken 项目工作协议

> 本文件对本项目的所有开发工作（包括 AI 助手与本仓库后续会话）具有约束力。
> **核心约定：做完必更新计划，反馈必回写文档。**

## 1. 项目定位

**OneToken** 是一个 Go 实现的 CLI 工具：基于论文《One Token Is Enough》（arXiv:2607.10252）的单 token 行为指纹方法，对"某提供商的某模型端点"做**黑盒真伪检测**（模型替换 / 量化顶替 / 版本回退 / 跨 provider 漂移）。

- **实现语言**：Go（用户拍板；IO 密集场景，启动毫秒级、单二进制、goroutine 并发）
- **当前状态**：设计阶段，尚未开始实现代码
- **环境**：WSL2 (Ubuntu 22.04)，项目位于 `/home/acidmoon/projects/OneToken`；在 Windows VSCode 中需用 WSL Remote 打开（`\\wsl$\Ubuntu\home\acidmoon\projects\OneToken`）

## 2. 文档地图（必读与维护）

| 文档 | 角色 | 何时更新 |
|---|---|---|
| `docs/OneToken-Is-Enough-LLM-Fingerprinting-2607.10252.pdf` | 论文原件 | 只读，不修改 |
| `docs/OneToken-Is-Enough-论文总结.md` | 论文要点与方法论（设计的事实依据） | 论文相关核对有出入时修正 |
| `docs/OneToken-工程设计方案.md` | **工程设计与验收标准**（当前 v0.4；所有验收以它为准） | 设计决策变更时必须更新，并 bump 版本号 + 在 §7 决策日志留痕 |
| `docs/OneToken-实施计划.md` | **项目进度唯一真相**：P0+M1–M4 任务拆分、状态、风险、决策日志 | **每次完成任何实质性工作后必须更新**（勾选状态/日期/备注/追加日志） |
| `AGENTS.md` | 本工作协议 | 工作流约定变更时 |

**文档依赖方向**：论文总结 → 工程设计方案（验收依据）→ 实施计划（进度跟踪）。三者不一致时：进度以实施计划为准，验收以设计方案为准，不一致处必须在对应文档显式标注原因。

## 3. 强制工作流规则

### 3.1 每次任务完成后（必做）

1. **更新 `docs/OneToken-实施计划.md`**：
   - 完成的任务：⬜/🔄 → ✅，填完成日期与备注；
   - 阻塞/变更：⛔ 注明原因；
   - 新发现的风险、依赖或任务拆分调整：同步更新 §6 风险表与任务表；
   - 追加 §8 更新日志（日期/变更内容）。
2. **按验收标准自检**：对照设计文档对应里程碑的验收项逐条勾选；未过项记入"下一迭代"，不得静默跳过。
3. **实质工作（代码/文档改动）完成后执行对抗式审查**：至少并行启动正确性、安全性、需求符合性三个视角的独立审查代理；确认的问题修复后再交付，未采纳的意见在回复中说明理由。

### 3.2 收到反馈/评审意见后（必做）

1. **先回写文档，再改代码**：用户反馈或审查意见到达时，先把结论落入对应文档——
   - 影响设计/协议/验收的 → 更新 `docs/OneToken-工程设计方案.md`（bump 版本 + §7 决策日志）；
   - 影响任务拆分/状态/风险 → 更新 `docs/OneToken-实施计划.md`（必要时追加 §7 决策日志与 §8 更新日志）；
   - 影响论文事实依据 → 更新 `docs/OneToken-Is-Enough-论文总结.md`。
2. 文档更新完成后，再开始实现改动。
3. 本文件 `AGENTS.md` 自身变更时同样记录。

### 3.3 开工前（必做）

1. 读 `AGENTS.md` + `docs/OneToken-实施计划.md`，确认当前阶段与任务状态；
2. 只做计划中标记为 ⬜ 且前置依赖已完成的任务；不在计划中的工作先补进计划再动手；
3. 涉及验收口径的疑问先对照 `docs/OneToken-工程设计方案.md`，再决定是否问用户。

## 4. 关键技术约束（实现时必须遵守）

- **统一提供商调用层是系统核心**：任意端点 = `ProviderConfig{name, base_url, api_key, protocol}`；`base_url` **不含 `/v1`**，层内统一拼 `/v1/<endpoint>`；enroll/audit 同构走同一管线。
- **三协议**：Responses（`reasoning:{effort:"minimal"}`，o 系拒绝时降级 low 或按 §1.2 排除、`reasoning_tokens`）/ chat（顶层 `reasoning_effort`、`completion_tokens_details`）/ Anthropic（`thinking:{type:"disabled"}`，无需 beta 头）。
- **JSD 基 2 自写**：`(KL(p‖m)+KL(q‖m))/(2·ln2)`，KL 自然对数，**0·ln0=0 无平滑**；常数标度以 M1 前置门（Zenodo 软件归档）pin 为准。
- **密钥**：只走环境变量（`api_key_env`），永不落盘/落日志/落报告。
- **SQLite**：modernc 纯 Go、`PRAGMA foreign_keys=ON + WAL + busy_timeout`、幂等用部分唯一索引（`idx_resp_idem_audit`/`idx_resp_idem_fp`）。
- **性能**：启动 <50ms（含 DB 打开与迁移）；120 查询审计典型 3–20s（网络主导），不承诺 <2s。
- **安全**：禁用重定向（`CheckRedirect → ErrUseLastResponse`）、scheme 校验（https，localhost 例外）、SSRF 拦截（RFC1918/环回/链路本地/CGNAT/IPv6 私有段 + DNS rebinding 解析→校验→拨号）、base_url↔密钥绑定校验、报告 HTML 默认转义。

## 5. 完成度检查清单（交付前过一遍）

- [ ] 实施计划对应任务已勾选/更新，§8 更新日志已追加
- [ ] 设计文档无未记录的设计变更（有变更则已 bump 版本 + §7 日志）
- [ ] 对抗式审查（正确性/安全性/需求符合性）已执行，确认问题已修复或已说明不采纳理由
- [ ] 密钥未进入任何文件/日志/报告
- [ ] 验收标准逐项核对过（以设计文档为准）
