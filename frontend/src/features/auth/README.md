# auth：浏览器认证流程

[中文](README.md) | [English](README.en.md)

## 职责

本模块以 Gateway HttpOnly Cookie 和 Cloud `/api/v1/me` 为唯一登录事实。它负责安全 `returnTo`、未登录时
自动创建 Gateway Login Attempt、跳转华为统一登录、当前用户探测和本地 Cloud 退出；不保存 token，
不解释租户权限，也不管理业务演示数据。

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 当前用户、登录开始、退出和稳定错误分类的 HTTP/TanStack Query adapter。 |
| `return-to.ts` | 与 Gateway 一致的站内返回路径约束。 |
| `login-page.tsx` | 自动登录过渡页；失败后停止循环并提供显式重试。 |
| `*.test.ts(x)` | 覆盖路径约束、自动跳转、失败停止、禁用账号和退出契约。 |

依赖生成的 `me` client、共享 axios adapter 和展示原语；路由与布局可以依赖本模块，低层模块不能反向
依赖页面。401 才能启动外部登录，403 不得形成登录循环，外部 URL 必须经响应校验后才交给浏览器。
