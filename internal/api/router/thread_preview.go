package router

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wanglongan587/cloud/internal/core"
)

// sseThreadPreviews sends replacement snapshots after rechecking current credentials and membership.
// Disconnect only releases the subscription. Coalesced notifications never backpressure the Agent.
func sseThreadPreviews(store *core.Store, auth *core.Authenticator, c *gin.Context) {
	if store.ThreadPreviews == nil {
		failure(c, &core.Fault{Status: 503, Code: "realtime_unavailable", Params: core.Object{}})
		return
	}
	tid, iid, rid := c.Param("tid"), c.Param("iid"), c.Param("rid")
	read := func() (core.Object, error) {
		_, user, fault := verifyPublicCredentials(c, auth)
		if fault != nil {
			return nil, fault
		}
		out, _, err := store.Public(c.Request.Context(), &core.PublicRequest{Method: "GET", Path: "/api/v1/tenants/" + tid + "/issues/" + iid + "/runs/" + rid + "/thread/preview", TenantID: tid, IssueID: iid, RunID: rid, Identity: user})
		return out, err
	}
	_, err := read()
	if err != nil {
		failure(c, core.ErrorCode(err))
		return
	}
	wake, cancel := store.ThreadPreviews.Subscribe(rid)
	defer cancel()
	if wake == nil {
		failure(c, &core.Fault{Status: 503, Code: "realtime_unavailable", Params: core.Object{}})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	write := func(snapshot core.Object) bool {
		body, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			return false
		}
		if _, writeErr := fmt.Fprintf(c.Writer, "data: %s\n\n", body); writeErr != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-wake:
		case <-tick.C:
		}
		// This read also drops settled blocks and detects terminal executions or expired caches.
		snapshot, readErr := read()
		if readErr != nil {
			write(core.Object{"available": false, "executionId": "", "version": 0, "incomplete": false, "blocks": []any{}})
			return
		}
		if !write(snapshot) {
			return
		}
	}
}
