# Cloud Go Backend Service

基于 Go 语言构建的标准企业级微服务/后端工程骨架，遵循社区规范 [golang-standards/project-layout](https://github.com/golang-standards/project-layout) 与 Clean Architecture 分层设计模式。

---

## 🛠 技术栈与核心特性

- **Web 框架**: [Gin](https://github.com/gin-gonic/gin)（高性能 HTTP 路由与中间件处理）
- **持久化 ORM**: [GORM](https://gorm.io/)（集成连接池管理、自动表结构迁移、支持 SQLite 与 MySQL 双驱动）
- **日志框架**: [Uber Zap](https://github.com/uber-go/zap) + [Lumberjack](https://github.com/natefinch/lumberjack)（结构化输出、日志切割与归档、终端色彩输出与文件 JSON 输出双引擎）
- **配置管理**: [Viper](https://github.com/spf13/viper)（YAML 配置文件与环境变量自动映射）
- **高可用与生命周期**: HTTP 优雅停机（Graceful Shutdown，监听系统退出信号平滑关闭连接与释放资源）
- **工程设计**: 统一 RESTful API JSON 响应封装、Zap 请求与 Panic Recovery 全局中间件、CORS 跨域支持

---

## 📁 目录规范说明

```text
.
├── cmd/
│   └── server/
│       └── main.go                 # 服务主入口：装配各层依赖、初始化基础设施、启动服务并监听停机信号
├── configs/
│   └── config.yaml                # 默认配置文件（服务器端口、数据库 DSN、日志级别及轮转策略）
├── internal/                       # 应用核心私有代码 (内部包，外部项目无法直接 import)
│   ├── api/
│   │   ├── handler/               # 控制器层 (Handler)：解析与校验 HTTP 入参，组装返回响应
│   │   ├── middleware/            # Gin 中间件：Zap 日志追踪、Panic 恢复、CORS
│   │   └── router/                # 路由注册：装配全局中间件与 API 路由分组
│   ├── config/                    # 配置结构体映射与加载逻辑
│   ├── model/                     # 业务实体 (Entity / DTO / GORM 映射模型)
│   ├── repository/                # 数据持久层 (DAO / Repository)：负责数据库 CRUD 与连接池维护
│   └── service/                   # 业务逻辑层 (Service)：核心业务规则编排
├── pkg/                            # 公共可复用包 (可供外部仓库或其它微服务共享)
│   ├── logger/                    # 基于 Zap + Lumberjack 封装的通用日志工具
│   └── response/                  # 统一 RESTful 响应格式封装
├── scripts/                        # 构建与运维脚本
│   ├── Dockerfile                 # 多阶段轻量级 Dockerfile
│   └── Makefile                   # 常用开发脚本 (build/run/test/clean)
├── go.mod                         # Go 依赖包管理文件
├── go.sum                         # 依赖校验哈希
└── README.md                      # 项目说明文档
```

---

## 🚀 快速开始

### 1. 安装依赖

确保本地已安装 Go (建议 1.20+)，在项目根目录下执行：

```bash
go mod tidy
```

### 2. 启动服务

```bash
# 方式一：直接运行
go run cmd/server/main.go

# 方式二：指定自定义配置文件
go run cmd/server/main.go -config configs/config.yaml

# 方式三：使用 Makefile
make run
```

服务默认在 `http://localhost:8080` 启动，并自动创建本地 SQLite 数据库文件 `cloud.db`。

---

## 📡 API 接口说明

| 请求方法 | 接口路径 | 描述 |
| :--- | :--- | :--- |
| `GET` | `/api/v1/health` | 服务健康检查探针 |
| `POST` | `/api/v1/users` | 创建用户 (JSON Body) |
| `GET` | `/api/v1/users` | 分页获取用户列表 (`?page=1&page_size=10`) |
| `GET` | `/api/v1/users/:id` | 根据用户 ID 查询用户详情 |

### 请求示例

#### 1. 健康检查
```bash
curl -X GET http://localhost:8080/api/v1/health
```
响应：
```json
{
  "code": 0,
  "message": "success",
  "data": {
    "service": "cloud-backend",
    "status": "UP",
    "timestamp": "2026-09-08T17:28:00+08:00"
  }
}
```

#### 2. 创建用户
```bash
curl -X POST http://localhost:8080/api/v1/users \
  -H "Content-Type: application/json" \
  -d '{"username": "developer", "nickname": "Coder", "email": "dev@example.com"}'
```

---

## ⚙️ 配置说明 (`configs/config.yaml`)

```yaml
server:
  port: 8080
  mode: "debug"              # debug / release / test
  read_timeout: 10           # 读超时 (秒)
  write_timeout: 10          # 写超时 (秒)

logger:
  level: "info"              # 日志级别: debug / info / warn / error
  filename: "logs/app.log"   # 日志持久化路径
  max_size: 100              # 单个日志文件最大尺寸 (MB)
  max_backups: 10            # 最多保留旧日志文件数
  max_age: 30                # 保留天数
  compress: true             # 是否 gzip 压缩旧日志
  enable_console: true       # 是否同时输出至终端控制台

database:
  driver: "sqlite"           # 支持 sqlite 或 mysql
  dsn: "cloud.db"            # 数据库连接串
  max_idle_conns: 10         # 最大空闲连接数
  max_open_conns: 100        # 最大打开连接数
  conn_max_lifetime: 3600    # 连接可复用的最大时间 (秒)
  auto_migrate: true         # 启动时是否自动迁移建表
```

> **提示**：若切换至 MySQL，仅需将 `driver` 改为 `mysql`，并将 `dsn` 修改为类似 `"user:password@tcp(127.0.0.1:3306)/cloud?charset=utf8mb4&parseTime=True&loc=Local"`。

---

## 🔍 代码规范与质量工程 (Format & Lint)

本项目引入了 Go 社区最严格、最高标准的工程化质量保证体系（对标 Rust 的 `cargo fmt` 与 `cargo clippy`）：

| 维度 | Rust 工具生态 | Go 对应方案（本项目采用） | 说明 |
| :--- | :--- | :--- | :--- |
| **代码格式化** | `rustfmt` | **`gofumpt`** + **`goimports`** | 比默认 `gofmt` 更严格的语法与空行规范，自动排序与分组 package import |
| **静态分析与代码检查** | `clippy` (`clippy.toml`) | **`golangci-lint`** (`.golangci.yml`) | 业界统治级多引擎 Linter（集成 govet、errcheck、staticcheck、revive、gocritic、gosec 等 10+ 款检查器） |
| **任务命令编排** | `cargo` / `Taskfile` | **`go-task`** (`Taskfile.yml`) + `Makefile` | 跨平台 Task 命令，无缝适配 Windows / macOS / Linux |
| **编辑器统一规范** | `.editorconfig` | **`.editorconfig`** | 强制 Go 统一使用 Hard Tabs，缩进宽度为 4，文件末尾换行 |
| **CI 持续集成** | GitHub Actions | **`.github/workflows/ci.yml`** | 提交代码或 PR 时自动执行全量格式校验、静态检查与竞态测试 |

### 常用质量命令 (通过 Task 或 Make)

```bash
# 1. 自动格式化代码 (对标 cargo fmt)
task fmt
# 或
make fmt

# 2. 静态代码分析与异味检查 (对标 cargo clippy)
task lint
# 或
make lint

# 3. 自动修复可修复的 lint 警告
task lint:fix

# 4. 全量质量门禁 (格式校验 + 静态分析 + 单元测试，推荐作为 pre-commit 检查)
task check
# 或
make check
```

---

## 🧪 测试与构建

```bash
# 执行单元测试
task test
# 或
make test

# 编译为生产二进制包
task build
# 或
make build

# 构建 Docker 容器镜像
make docker-build
```