// Package logger provides process-scoped structured logging via Uber Zap and Lumberjack.
package logger

import (
	"errors"
	"os"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Config defines logger configuration parameters.
type Config struct {
	Level         string `mapstructure:"level" json:"level" yaml:"level"`
	Filename      string `mapstructure:"filename" json:"filename" yaml:"filename"`
	MaxSize       int    `mapstructure:"max_size" json:"max_size" yaml:"max_size"`                   // in megabytes
	MaxBackups    int    `mapstructure:"max_backups" json:"max_backups" yaml:"max_backups"`          // max number of old log files
	MaxAge        int    `mapstructure:"max_age" json:"max_age" yaml:"max_age"`                      // in days
	Compress      bool   `mapstructure:"compress" json:"compress" yaml:"compress"`                   // compress old files with gzip
	EnableConsole bool   `mapstructure:"enable_console" json:"enable_console" yaml:"enable_console"` // write to stdout as well
}

// New constructs an independently owned zap logger with Lumberjack rotation.
func New(cfg Config) (*zap.Logger, error) {
	var level zapcore.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalColorLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var cores []zapcore.Core

	// Console Core
	if cfg.EnableConsole {
		consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)
		cores = append(cores, zapcore.NewCore(consoleEncoder, zapcore.Lock(os.Stdout), level))
	}

	// File Core with Lumberjack rotation
	if cfg.Filename != "" {
		fileEncoderConfig := encoderConfig
		fileEncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder // non-color for file

		writer := &lumberjack.Logger{
			Filename:   cfg.Filename,
			MaxSize:    cfg.MaxSize,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAge,
			Compress:   cfg.Compress,
		}

		fileEncoder := zapcore.NewJSONEncoder(fileEncoderConfig)
		cores = append(cores, zapcore.NewCore(fileEncoder, zapcore.AddSync(writer), level))
	}

	core := zapcore.NewTee(cores...)
	return zap.New(core, zap.AddCaller()), nil
}

// Sync flushes buffered entries from log. Windows console handles can report EINVAL after a
// successful flush, so that platform error is treated as a completed sync.
func Sync(log *zap.Logger) error {
	if log == nil {
		return nil
	}
	if err := log.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}
