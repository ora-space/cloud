# chat：独立聊天入口

独立聊天尚未开放；列表和旧会话 URL 都显示明确的不可用说明，引导到真实任务的 Agent Thread。不展示联系人、历史或输入框，也不发起演示聊天请求。

| 文件 | 职责 |
| --- | --- |
| `chat-page.tsx` | 保留聊天路径并引导到当前工作区的任务 |
| `chat-page.test.tsx` | 列表和旧详情路径不渲染演示会话 |

依赖 unavailable、lib/paths；由路由组合根消费。真实会话属于 issues/thread，本模块不承担执行或消息存储。
