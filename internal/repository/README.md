# internal/repository: PostgreSQL Connection & Pool Management

`internal/repository` owns the initialization, configuration, and verification of PostgreSQL connection pools for Ora Cloud.

## Responsibilities

- **Connection setup**: `InitDB` establishes the connection to PostgreSQL using GORM's PostgreSQL driver with the configured DSN.
- **Connection pooling**: Configures standard database pool tuning parameters from `config.DatabaseConfig`:
  - `SetMaxOpenConns`: Caps maximum concurrent open connections.
  - `SetMaxIdleConns`: Maintains an optimal number of idle connections.
  - `SetConnMaxLifetime`: Enforces connection turnover to respect database server connection policies.
- **Fail-fast health verification**: Performs `pool.PingContext(ctx)` during initialization. If the database is unreachable, the pool is closed immediately and a wrapped descriptive error is returned, preventing half-initialized daemon startup.
- **Silent GORM logging**: Silences GORM's internal loggers (`logger.Silent`). Application-level operational and error logging is owned exclusively by `internal/logger` and request lifecycle middleware.

## Boundaries and invariants

- **PostgreSQL exclusively**: `cfg.Driver` must equal `"postgres"`. In-memory databases, SQLite, MySQL, or mock database layers are rejected.
- **No schema mutations**: This package does **not** invoke GORM's `AutoMigrate` or issue DDL. Schema definitions and updates belong exclusively to `internal/core/migrations` and `cloudctl migrate`.
- **No domain queries**: Data access logic and transaction orchestration belong to `internal/core`. This package only delivers a verified `*gorm.DB` instance to `core.NewStore`.

See [Core store](../core/README.md), [Database migrations](../core/migrations/README.md), and [Configuration](../config/README.md).
