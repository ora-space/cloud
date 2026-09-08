// Package config handles application configuration loading and parsing.
package config

import (
	"strings"

	"github.com/spf13/viper"

	"github.com/wanglongan587/cloud/pkg/logger"
)

// Config holds all configuration of the application
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Logger   logger.Config  `mapstructure:"logger"`
	Database DatabaseConfig `mapstructure:"database"`
}

// ServerConfig holds HTTP server configuration
type ServerConfig struct {
	Port         int    `mapstructure:"port"`
	Mode         string `mapstructure:"mode"`
	ReadTimeout  int    `mapstructure:"read_timeout"`
	WriteTimeout int    `mapstructure:"write_timeout"`
}

// DatabaseConfig holds database connection parameters
type DatabaseConfig struct {
	Driver          string `mapstructure:"driver"`
	DSN             string `mapstructure:"dsn"`
	MaxIdleConns    int    `mapstructure:"max_idle_conns"`
	MaxOpenConns    int    `mapstructure:"max_open_conns"`
	ConnMaxLifetime int    `mapstructure:"conn_max_lifetime"`
	AutoMigrate     bool   `mapstructure:"auto_migrate"`
}

// Load reads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	v := viper.New()

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("../configs")
		v.AddConfigPath(".")
	}

	// Read environment variables (e.g., CLOUD_SERVER_PORT=8080)
	v.SetEnvPrefix("CLOUD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
