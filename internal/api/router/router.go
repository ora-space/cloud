// Package router initializes and registers application routes.
package router

import (
	"github.com/gin-gonic/gin"

	"github.com/wanglongan587/cloud/internal/api/handler"
	"github.com/wanglongan587/cloud/internal/api/middleware"
	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/repository"
	"github.com/wanglongan587/cloud/internal/service"
)

// InitRouter initializes the Gin router and registers all routes
func InitRouter(cfg *config.Config) *gin.Engine {
	if cfg.Server.Mode != "" {
		gin.SetMode(cfg.Server.Mode)
	}

	r := gin.New()

	// Global Middlewares
	r.Use(middleware.CORS())
	r.Use(middleware.ZapLogger())
	r.Use(middleware.ZapRecovery())

	// Dependency Injection / Layer Assembly
	userRepo := repository.NewUserRepository(repository.DB)
	userService := service.NewUserService(userRepo)
	userHandler := handler.NewUserHandler(userService)
	healthHandler := handler.NewHealthHandler()

	// API Route Group
	v1 := r.Group("/api/v1")
	{
		// Health Check
		v1.GET("/health", healthHandler.Check)

		// User Routes
		users := v1.Group("/users")
		users.POST("", userHandler.CreateUser)
		users.GET("", userHandler.ListUsers)
		users.GET("/:id", userHandler.GetUser)
	}

	return r
}
