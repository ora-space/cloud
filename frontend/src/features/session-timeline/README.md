# session-timeline：会话事件浏览

[English](README.en.md) | 中文

以原 Dashboard 的三泳道、框选、事件列表及五页详情浏览已经归一化的 Cloud 会话；不负责请求、归档下载、provider 解析或个人日志扫描。

| 文件 | 职责 |
| --- | --- |
| `index.ts` | 对外提供 `SessionTimeline`、`SessionRecords`、`ToolSummary`，均接收 `SessionSnapshot` |
| `session-timeline.tsx` | 三类筛选、全文搜索与时间范围组合 |
| `timeline-strip.tsx`、`use-time-brush.ts` | 三条真实时间泳道、独立缩放视窗、指针捕获/取消与键盘滑块 |
| `compact-timeline.tsx`、`timeline-controls.tsx` | 默认等距事件布局、真实时间模式切换和视窗操作 |
| `event-list.tsx`、`event-workspace.tsx`、`use-event-selection.ts` | 可继续加载的列表、轮次分组及稳定 ID 选中 |
| `event-details.tsx` | 摘要、Payload、Result、Timing、JSON 只读详情 |
| `session-records.tsx` | 所有类别检索及重放前的完整原始记录 |
| `tool-summary.tsx`、`timeline-values.ts` | 工具类别统计与保守的显示格式 |
| `session-timeline.test.tsx`、`timeline-strip.test.tsx` | 合成数据验证筛选、框选、缩放、布局、详情和选中稳定性 |

依赖 `features/session-data` 的公开类型与 `components/ui`；会话页面可依赖本模块。未知时间记录不放入泳道，但框选时仍保留在列表。未知耗时/用量显示未知，真实零耗时显示 `0ms`；相邻倒序时间不补零，也不推算模型延迟。详情按事件 ID 绑定，筛选隐藏及同一 Run 刷新不会错配；切换 Run 重置交互状态。模型文本数不是 LLM 调用数，工具类别不是原生工具名。只读原始 JSON 通过 React 文本渲染。测试不使用真实日志、不访问网络。

工具事件以已报告的类别显示操作（如“读取 · Cargo.toml”），文件名或命令标题只作为操作说明，不作为工具名称；未知类别显示“工具调用”。名称在列表、泳道提示与详情中保持一致，详情另保留原始标题。

默认“紧凑事件”按真实开始时间对副本稳定排序，等距列和固定宽度块只表达事件顺序，明确不代表耗时；原始列表和轮次仍按来源 seq。“真实时间”保留时间比例，可框选后放大、显示全程；缩放视窗独立于筛选，不把窗口外事件挤在两端，跨窗口工具按真实区间裁剪，详情保留完整耗时。框选也包含跨区间工具，未知开始时间仍留在列表。亚秒视窗使用毫秒刻度，禁止用相邻事件间隔填充未知耗时。
