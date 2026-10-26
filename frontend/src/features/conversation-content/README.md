# 对话正文

[English](README.en.md) | 中文

安全展示用户和 Agent 的 Markdown 正文；不负责请求、消息存储和执行路由。

| 文件 | 职责 |
| --- | --- |
| `markdown.tsx` | `ConversationMarkdown`：GFM、代码块、表格与安全链接。 |
| `clipboard.ts` | 显式复制代码的浏览器边界；失败由正文组件显示。 |
| `markdown.test.tsx` | 真实渲染器的格式与恶意内容回归测试。 |

依赖 React、react-markdown、remark-gfm；任务与会话展示可依赖本模块。禁用原始 HTML，图片只显示说明以避免加载第三方资源；链接由 Markdown 的默认安全 URL 转换处理。长代码和表格仅在各自区域横向滚动。测试无网络。
