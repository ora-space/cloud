package handler

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wanglongan587/cloud/pkg/response"
)

// HealthHandler handles health check endpoint
type HealthHandler struct{}

// NewHealthHandler creates a new HealthHandler
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// Check returns service health status
func (h *HealthHandler) Check(c *gin.Context) {
	data := gin.H{
		"status":    "UP",
		"timestamp": time.Now().Format(time.RFC3339),
		"service":   "cloud-backend",
	}
	response.Success(c, data)
}
