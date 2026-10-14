# agents：智能体管理入口

独立智能体管理尚未开放；列表和旧详情链接显示不可用说明，指向真实插件安装和任务入口，不读取演示 Agent、虚构在线状态或分配任务。

| 文件 | 职责 |
| --- | --- |
| `agents-page.tsx` | 管理入口说明与当前工作区插件、任务导航 |
| `agent-detail-page.tsx` | 旧 Agent 详情 URL 复用管理说明 |
| `*.test.tsx` | 入口、旧链接及演示数据排除验证 |

依赖 unavailable、lib/paths；由路由组合根消费。可执行身份由 issues 的真实 collaboration targets 提供，本模块不能构造或管理执行身份。
