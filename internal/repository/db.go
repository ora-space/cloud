// Package repository owns PostgreSQL connections. Schema changes run via cloudctl migrate.
package repository

import (
	"context"
	"fmt"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/config"
)

// InitDB opens and verifies a pool without performing schema mutations.
func InitDB(ctx context.Context, cfg config.DatabaseConfig) (*gorm.DB, error) {
	if cfg.Driver != "postgres" {
		return nil, fmt.Errorf("database.driver must be postgres")
	}
	db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get PostgreSQL pool: %w", err)
	}
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if err = pool.PingContext(ctx); err != nil {
		if closeErr := pool.Close(); closeErr != nil {
			return nil, fmt.Errorf("ping PostgreSQL: %w (close pool: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return db, nil
}
