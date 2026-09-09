package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUnexpectedPanicIncludesValueAndStackInLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logCore, recorded := observer.New(zap.ErrorLevel)
	engine := New(nil, nil, zap.New(logCore))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer triggers-nil-authenticator")

	engine.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	entries := recorded.FilterMessage("request panic").All()
	if len(entries) != 1 {
		t.Fatalf("expected one panic log, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["panic"] == nil || fields["stack"] == "" {
		t.Fatalf("panic log omitted value or stack: %v", fields)
	}
}
