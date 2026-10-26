package router

import (
	"mime"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestScheduledArtifactIsAlwaysASafeAttachment(t *testing.T) {
	for _, tc := range []struct{ name, filename string }{
		{"reports/result.html", "result.html"},
		{"..\\reports\\result.html", "result.html"},
		{"reports/检查\"结果.html", "检查\"结果.html"},
		{"reports/result\r\nInjected: yes.html", "resultInjected: yes.html"},
		{"..", "artifact"},
		{"", "artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			data := []byte("<script>alert(1)</script>\x00")
			writeScheduledArtifact(c, &core.ScheduledArtifactDownload{Name: tc.name, Data: data})
			kind, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if err != nil || kind != "attachment" || params["filename"] != tc.filename {
				t.Fatalf("disposition = %v %v %v", kind, params, err)
			}
			for name, want := range map[string]string{"Content-Type": "application/octet-stream", "X-Content-Type-Options": "nosniff", "Cache-Control": "private, no-store"} {
				if got := w.Header().Get(name); got != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
			if w.Code != 200 || w.Body.String() != string(data) {
				t.Fatalf("response = %d %q, want exact bytes", w.Code, w.Body.String())
			}
		})
	}
}
