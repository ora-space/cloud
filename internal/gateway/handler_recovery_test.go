package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestRecoveryLetsExpectedAbortReachHTTPServer keeps normal SSE disconnects out of error logs.
func TestRecoveryLetsExpectedAbortReachHTTPServer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logCore, recorded := observer.New(zap.ErrorLevel)
	h := &handler{Options: Options{Log: zap.New(logCore)}}
	router := gin.New()
	router.Use(h.recovery)
	router.GET("/events", func(c *gin.Context) {
		c.Writer.WriteHeaderNow()
		panic(http.ErrAbortHandler)
	})
	response := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("recovered = %v, want ErrAbortHandler", recovered)
		}
		if entries := recorded.All(); len(entries) != 0 {
			t.Fatalf("expected abort logged as an error: %v", entries)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("committed status changed to %d", response.Code)
		}
	}()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/events", nil))
}
