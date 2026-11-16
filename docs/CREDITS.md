# 借鉴来源登记

## kagent Chat 输出与交互（方案 25 / ADR-055）

- 来源：kagent，Apache-2.0；本地核查提交 `e3624635`（完整提交哈希以该次核查记录为准，不宣称随上游更新）。
- 复用方式：只借鉴消息/交互的**设计与纯数据转换语义**，不引入 ADK Runner、A2A Executor/TaskStore、CRD/Controller 或 React 页面；Go/Vue 实现在本项目内独立完成。
- `go/adk/pkg/a2a/executor.go`：借鉴 partial 与完整消息区分、工具内容按调用 ID 关联、`input-required` 不等于 completed；对应实现为 `internal/sse/contract.go`（`Message.Status/Purpose`、`ToolCall.ToolCallID`、`Interaction.Status`）与 `internal/agent/flow.go`。
- `ui/src/lib/messageHandlers.ts`、`chatSessionGuard.ts`：借鉴增量归并、已答交互回看、动作前核对当前交互、历史/实时稳定 ID；对应实现为 `web/src/chat/store.ts`（纯 reducer：重复 seq 忽略、缺口要求重同步、同 ID upsert）。
- `go/adk/pkg/tools/ask_user.go`、`AskUserDisplay.tsx`：借鉴结构化问题（选项 + 影响 + 自由输入/跳过）与回答后继续；对应实现为 `internal/agent/question.go`、`internal/sessionapi/question.go`、`web/src/components/chat/QuestionInteraction.vue`。未复制 Google ToolConfirmation 依赖，回答仍然不构成执行授权。
- HolmesGPT `holmes/core/tool_calling_llm.py`、`holmes/utils/stream.py`：借鉴过程说明与最终答案分离、按工具调用 ID 发开始/结果、统一错误收尾；对应实现为 Agent 每个模型调用一条 Message、`message.failed` + `notice.created`。未引入 Python 依赖或整段历史随终态反复下发。

## HolmesGPT（P10）

- 来源：HolmesGPT，Apache-2.0。
- 固定提交：`27c623ee3ab074734cf0d72964bafd11245b5f80`。
- 已研究：`holmes/core/tool_calling_llm.py`、`holmes/core/truncation/input_context_window_limiter.py`、`holmes/core/truncation/compaction.py`、`holmes/core/tools_utils/tool_context_window_limiter.py`。
- `holmes/plugins/prompts/conversation_history_compaction.jinja2` → `internal/agent/prompts/conversation_history_compaction.txt`：直接搬运主要结构与八个摘要章节；移除输出分析和服从历史内嵌指令的要求，追加 Secret、授权和未知写结果边界。原文件无单独版权头，完整 Apache-2.0 许可证随附 `internal/agent/prompts/HOLMESGPT-LICENSE`。
- `input_context_window_limiter.py`、`compaction.py` → `internal/agent/context.go`：按请求前预算、滚动摘要、长文本分段、压缩后复查的原顺序移植 Go；适配保守 UTF-8 字节估算、总次数/超时、现有消息契约和 Secret 隔离，未复制 Python SDK/Sentry/Robusta 集成。
- `tool_context_window_limiter.py` → `internal/agent/results.go`：移植大结果预览及按需读取模式；替换上游 shell 文件读取为当前轮不透明引用、源资源复核、结构化分页，使用既有对象网关与 GC。
- 失败反馈沿用我方既有 `tools.Present` 错误码，补充失败后的模型指导与受控重试，不宣称直接复制上游错误代码。评测和安全回归测试为我方新增，不冒充上游测试移植。

其他历史借鉴声明仍以各源文件现有许可证头及既有文档记录为准。

## DeepSeek Harness 用户决策交互

- 来源：https://github.com/deepseek-ai/deepseek-harness ，MIT，Copyright (c) 2026 DeepSeek。
- 2026-11-14 读取 master 的 packages/interaction/tool-ask-user/src/timed.ts；未取得固定提交 ID，不宣称已固定版本。
- 直接复制 description 的前两句到 internal/agent/prompts/user_question_guidance.txt；完整许可见同目录 DSH-LICENSE。复制内容随仓库保存，不在运行时下载。
- 工具调用后等待答案、选项影响说明、自由输入与跳过、输入区问题卡为设计借鉴。Go 协议与 Vue 组件按本项目独立实现；没有复制 Cordis 运行时、定时继续和跨进程恢复代码。
- AegisOps 的提问不授权写操作，未决状态阻止当前运行继续工具调用。
