# Thread 实时预览

[English](README.en.md) | 中文

本模块拥有运行级临时预览的网络边界、版本采用和最终记录覆盖。内容保存在 TanStack Query，离开最后一个订阅者后释放；断流不停止 Agent。

| 文件 | 职责 |
| --- | --- |
| `preview.ts` | 验证快照，拒绝重复/乱序版本，并按历史位置与持久记录合并。 |
| `use-thread-preview.ts` | 受取消约束的订阅、重连、权限拒绝清理和连接状态。 |
| `*.test.ts(x)` | 版本、最终覆盖和订阅生命周期验证。 |

依赖生成 API、`lib/event-stream` 和 TanStack Query；任务 Thread 展示可依赖本模块。每次收到有效快照会重置断线重试退避，让长期运行中凭据轮换后的重连保持短暂。临时数据不写回持久 Thread 缓存，不触发每片段全量读取。
