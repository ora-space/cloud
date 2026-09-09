// Package repository provides data access and database persistence layer.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/model"
	"github.com/wanglongan587/cloud/pkg/logger"
)

// DB is the global database instance
var DB *gorm.DB

// InitDB initializes database connection and pool
func InitDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch cfg.Driver {
	case "mysql":
		dialector = mysql.Open(cfg.DSN)
	case "sqlite":
		dialector = sqlite.Open(cfg.DSN)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Driver)
	}

	gormConfig := &gorm.Config{
		Logger: NewGormZapLogger(logger.Log),
	}

	db, err := gorm.Open(dialector, gormConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get sql.DB: %w", err)
	}

	// Set connection pool
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	}

	// Auto migration
	if cfg.AutoMigrate {
		if err := db.AutoMigrate(&model.User{}); err != nil {
			return nil, fmt.Errorf("failed to auto migrate: %w", err)
		}
		logger.Log.Info("Database auto migration completed")
	}

	DB = db
	return db, nil
}

// GormZapLogger integrates zap with gorm
type GormZapLogger struct {
	ZapLogger     *zap.Logger
	LogLevel      gormlogger.LogLevel
	SlowThreshold time.Duration
}

// NewGormZapLogger creates a GormZapLogger instance
func NewGormZapLogger(l *zap.Logger) gormlogger.Interface {
	return &GormZapLogger{
		ZapLogger:     l,
		LogLevel:      gormlogger.Info,
		SlowThreshold: 200 * time.Millisecond,
	}
}

func (l *GormZapLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.LogLevel = level
	return &newLogger
}

func (l *GormZapLogger) Info(ctx context.Context, msg string, data ...any) {
	if l.LogLevel >= gormlogger.Info {
		l.ZapLogger.Sugar().Infof(msg, data...)
	}
}

func (l *GormZapLogger) Warn(ctx context.Context, msg string, data ...any) {
	if l.LogLevel >= gormlogger.Warn {
		l.ZapLogger.Sugar().Warnf(msg, data...)
	}
}

func (l *GormZapLogger) Error(ctx context.Context, msg string, data ...any) {
	if l.LogLevel >= gormlogger.Error {
		l.ZapLogger.Sugar().Errorf(msg, data...)
	}
}

func (l *GormZapLogger) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if l.LogLevel <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.LogLevel >= gormlogger.Error:
		l.ZapLogger.Error("gorm trace error",
			zap.Error(err),
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql),
		)
	case elapsed > l.SlowThreshold && l.SlowThreshold != 0 && l.LogLevel >= gormlogger.Warn:
		l.ZapLogger.Warn("gorm slow sql query",
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql),
		)
	case l.LogLevel >= gormlogger.Info:
		l.ZapLogger.Debug("gorm sql query",
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql),
		)
	}
}
