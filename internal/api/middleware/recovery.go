package middleware

import (
	"net"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/pkg/logger"
	"github.com/wanglongan587/cloud/pkg/response"
)

// ZapRecovery is a Gin middleware that recovers from panics and logs with Zap
func ZapRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Check for broken pipe
				var brokenPipe bool
				if ne, ok := err.(*net.OpError); ok {
					if se, ok := ne.Err.(*os.SyscallError); ok {
						if strings.Contains(strings.ToLower(se.Error()), "broken pipe") ||
							strings.Contains(strings.ToLower(se.Error()), "connection reset by peer") {
							brokenPipe = true
						}
					}
				}

				if brokenPipe {
					logger.Log.Error("Broken pipe error",
						zap.Any("error", err),
						zap.String("path", c.Request.URL.Path),
					)
					if e, ok := err.(error); ok {
						_ = c.Error(e)
					}
					c.Abort()
					return
				}

				logger.Log.Error("Panic recovered",
					zap.Any("error", err),
					zap.String("path", c.Request.URL.Path),
					zap.Stack("stack"),
				)

				response.ServerError(c, "Internal server error")
				c.Abort()
			}
		}()
		c.Next()
	}
}
