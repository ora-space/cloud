package router

import (
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/wanglongan587/cloud/internal/core"
)

func serveScheduledArtifact(c *gin.Context, store *core.Store, req *core.PublicRequest) {
	c.Header("Cache-Control", "private, no-store")
	artifact, err := store.SessionRunArtifact(c.Request.Context(), req)
	if err != nil {
		failure(c, core.ErrorCode(err))
		return
	}
	writeScheduledArtifact(c, artifact)
}

// Both separator styles are removed independent of the Cloud host OS. MIME encoding keeps quotes,
// Unicode and untrusted display names out of header syntax; active content is always downloaded.
func writeScheduledArtifact(c *gin.Context, artifact *core.ScheduledArtifactDownload) {
	name := path.Base(strings.ReplaceAll(artifact.Name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "artifact"
	}
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, "application/octet-stream", artifact.Data)
}
