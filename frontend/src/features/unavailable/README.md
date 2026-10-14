# unavailable：未开放入口说明

保留产品入口和旧链接，明确说明功能暂不可用，并提供调用者指定的真实工作区路径。模块不查询目录、不生成演示记录、不接受未开放功能的输入。

| 文件 | 职责 |
| --- | --- |
| `unavailable-page.tsx` | 通用说明页与可用入口按钮 |
| `unavailable-page.test.tsx` | 说明、导航和无演示请求的行为验证 |

依赖 layout、ui 与 React Router；由 chat、agents 页面消费。调用者负责生成当前工作区的合法路径，不能把 tenant UUID 当作 workspace slug。
