# cmd/ora-web：本地前端开发入口

[中文](README.md) | [English](README.en.md)

为 `frontend/` 提供本地 HTTP 入口，装配真实 Cloud router、开发登录和协作夹具。它不是生产 Gateway。

## 数据与入口

- `ORA_DATABASE_URL` 指定 PostgreSQL；默认连接本地 `127.0.0.1:55432` 的 `ora` 数据库（连接参数见 `main.go`）。
- 创建并保留固定 `ora_web` schema，启动时执行内嵌迁移并确保开发租户存在，重启不清数据。
- `POST /auth/login` 根据输入的 email 选择或建立开发成员；`POST /auth/register` 创建独立租户；`POST /auth/logout` 清 Cookie。这些是开发身份入口，没有外部身份核验。
- `/api/` 请求由服务端根据 `ora_subject` Cookie 签发 service/user JWT 后交给 Cloud router；Cookie 是开发用 subject，并非生产会话凭据。
- `ORA_WEB_ADDR` 默认 `127.0.0.1:8080`。`ORA_WEB_DEV=1` 将前端请求重定向至 `http://localhost:5173`；否则从 `frontend/dist` 提供 SPA 和客户端路由回退。

从仓库根目录运行 `go run ./cmd/ora-web`；静态模式需先构建前端。收到退出信号后关闭 HTTP 服务与数据库连接。

## 开发边界

密钥每次在内存生成；[collab](../../internal/collab/README.md) 夹具无条件接入，仅用于演示。生产登录与会话应使用 [gateway](../gateway/README.md)，本地开发的常规步骤见 [本地开发](../../docs/development/local-development.md)。
