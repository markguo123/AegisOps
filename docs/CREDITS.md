# 借鉴来源登记

## HolmesGPT（P10）

- 来源：HolmesGPT，Apache-2.0。
- 固定提交：`27c623ee3ab074734cf0d72964bafd11245b5f80`。
- 已研究：`holmes/core/tool_calling_llm.py`、`holmes/core/truncation/input_context_window_limiter.py`、`holmes/core/truncation/compaction.py`、`holmes/core/tools_utils/tool_context_window_limiter.py`。
- `holmes/plugins/prompts/conversation_history_compaction.jinja2` → `internal/agent/prompts/conversation_history_compaction.txt`：直接搬运主要结构与八个摘要章节；移除输出分析和服从历史内嵌指令的要求，追加 Secret、授权和未知写结果边界。原文件无单独版权头，完整 Apache-2.0 许可证随附 `internal/agent/prompts/HOLMESGPT-LICENSE`。
- `input_context_window_limiter.py`、`compaction.py` → `internal/agent/context.go`：按请求前预算、滚动摘要、长文本分段、压缩后复查的原顺序移植 Go；适配保守 UTF-8 字节估算、总次数/超时、现有消息契约和 Secret 隔离，未复制 Python SDK/Sentry/Robusta 集成。
- `tool_context_window_limiter.py` → `internal/agent/results.go`：移植大结果预览及按需读取模式；替换上游 shell 文件读取为当前轮不透明引用、源资源复核、结构化分页，使用既有对象网关与 GC。
- 失败反馈沿用我方既有 `tools.Present` 错误码，补充失败后的模型指导与受控重试，不宣称直接复制上游错误代码。评测和安全回归测试为我方新增，不冒充上游测试移植。

其他历史借鉴声明仍以各源文件现有许可证头及既有文档记录为准。
