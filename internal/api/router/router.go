// Package router binds typed routes to the cloud core and verifies two independent credentials.
package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
)

// Route describes the implemented contract, also used by the OpenAPI coverage test.
type Route struct {
	Method, Path, Action string
	Fields               []string
}

// Routes is an explicit allowlist. Unknown JSON properties cannot set server-owned bindings.
func Routes() []Route {
	return []Route{
		{"GET", "/api/v1/me", "", nil},
		{"GET", "/api/v1/me/tenants", "", nil},
		{"POST", "/api/v1/tenants", "", []string{"name", "slug"}},
		{"GET", "/api/v1/tenants/:tid/members", "", nil},
		{"PUT", "/api/v1/tenants/:tid/members/:uid", "", []string{"role", "status", "version"}},
		{"GET", "/api/v1/tenants/:tid/collaboration/targets", "", nil},
		{"GET", "/api/v1/tenants/:tid/collaboration/forms/:formRef", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues", "", []string{"title", "description", "status", "priority", "assigneeUserId", "assigneeType", "assigneeId", "parentIssueId", "projectRef", "properties"}},
		{"GET", "/api/v1/tenants/:tid/issues/:iid", "", nil},
		{"PUT", "/api/v1/tenants/:tid/issues/:iid", "", []string{"title", "description", "status", "priority", "assigneeUserId", "assigneeType", "assigneeId", "parentIssueId", "projectRef", "properties", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/issues/:iid", "", []string{"version"}},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/move", "", []string{"status", "beforeId", "afterId", "version"}},
		{"GET", "/api/v1/tenants/:tid/issue-statuses", "", nil},
		{"POST", "/api/v1/tenants/:tid/issue-statuses", "", []string{"key", "name", "description", "category", "color", "icon"}},
		{"PUT", "/api/v1/tenants/:tid/issue-statuses/:sid", "", []string{"name", "description", "category", "color", "icon", "position", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/issue-statuses/:sid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/labels", "", nil},
		{"POST", "/api/v1/tenants/:tid/labels", "", []string{"name", "color"}},
		{"PUT", "/api/v1/tenants/:tid/labels/:lid", "", []string{"name", "color", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/labels/:lid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/issue-views", "", nil},
		{"POST", "/api/v1/tenants/:tid/issue-views", "", []string{"name", "filter"}},
		{"PUT", "/api/v1/tenants/:tid/issue-views/:vid", "", []string{"name", "filter", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/issue-views/:vid", "", []string{"version"}},
		{"POST", "/api/v1/tenants/:tid/issues/batch", "", []string{"ids", "status", "priority", "assigneeUserId"}},
		{"GET", "/api/v1/tenants/:tid/issue-groups", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/comments", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/comments", "", []string{"body", "parentId", "targets"}},
		{"PUT", "/api/v1/tenants/:tid/issues/:iid/comments/:cid", "", []string{"body", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/issues/:iid/comments/:cid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/labels", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/labels", "", []string{"labelId"}},
		{"DELETE", "/api/v1/tenants/:tid/issues/:iid/labels/:lid", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/subscribers", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/subscribers", "", []string{"userId"}},
		{"DELETE", "/api/v1/tenants/:tid/issues/:iid/subscribers", "", []string{"userId"}},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/runs", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/runs", "", []string{"executorType", "executorId", "input"}},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/runs/:rid", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/context-refs", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/context-refs", "", []string{"refType", "refId"}},
		{"DELETE", "/api/v1/tenants/:tid/issues/:iid/context-refs/:crid", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/timeline", "", nil},
		{"GET", "/api/v1/tenants/:tid/issues/:iid/interactions", "", nil},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/collaboration/assist", "", []string{"targetId", "values"}},
		{"POST", "/api/v1/tenants/:tid/issues/:iid/interactions/:ixid/confirm", "", []string{"values", "contextRefs"}},
		{"GET", "/api/v1/tenants/:tid/projects", "", nil},
		{"POST", "/api/v1/tenants/:tid/projects", "", []string{"name", "repositoryUrl", "defaultBranch", "credentialRefId"}},
		{"GET", "/api/v1/tenants/:tid/projects/:pid", "", nil},
		{"PATCH", "/api/v1/tenants/:tid/projects/:pid", "", []string{"name", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/projects/:pid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/projects/:pid/workspaces", "", nil},
		{"POST", "/api/v1/tenants/:tid/projects/:pid/workspaces", "", []string{"title", "baseRef"}},
		{"GET", "/api/v1/tenants/:tid/workspaces/:wid", "", nil},
		{"POST", "/api/v1/tenants/:tid/workspaces/:wid/start", "", []string{"version"}},
		{"POST", "/api/v1/tenants/:tid/workspaces/:wid/stop", "", []string{"version"}},
		{"DELETE", "/api/v1/tenants/:tid/workspaces/:wid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/clones", "", nil},
		{"POST", "/api/v1/tenants/:tid/clones", "", []string{"requestId", "repository", "branch"}},
		{"GET", "/api/v1/tenants/:tid/clones/:cloneId", "", nil},
		{"GET", "/api/v1/tenants/:tid/operations/:oid", "", nil},
		{"POST", "/api/v1/tenants/:tid/operations/:oid/retry", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/resource-status", "", nil},
		{"POST", "/api/v1/tenants/:tid/workspaces/:wid/administrative-stop", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/spaces", "", nil},
		{"POST", "/api/v1/tenants/:tid/spaces", "", []string{"name", "slug", "description"}},
		{"GET", "/api/v1/tenants/:tid/spaces/:spaceId", "", nil},
		{"PATCH", "/api/v1/tenants/:tid/spaces/:spaceId", "", []string{"name", "description", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/spaces/:spaceId", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/spaces/:spaceId/members", "", nil},
		{"POST", "/api/v1/tenants/:tid/spaces/:spaceId/members", "", []string{"email"}},
		{"PUT", "/api/v1/tenants/:tid/spaces/:spaceId/members/:uid", "", []string{"role", "status", "version"}},
		{"DELETE", "/api/v1/tenants/:tid/spaces/:spaceId/members/:uid", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/spaces/:spaceId/projects", "", nil},
		{"POST", "/api/v1/tenants/:tid/spaces/:spaceId/projects", "", []string{"name", "repositoryUrl", "defaultBranch", "credentialRefId"}},
		{"GET", "/api/v1/tenants/:tid/spaces/:spaceId/plugins/catalog", "", nil},
		{"GET", "/api/v1/tenants/:tid/spaces/:spaceId/plugins", "", nil},
		{"POST", "/api/v1/tenants/:tid/spaces/:spaceId/plugins", "", []string{"identifier", "pluginVersion"}},
		{"DELETE", "/api/v1/tenants/:tid/spaces/:spaceId/plugins", "", []string{"identifier", "version"}},
		{"POST", "/internal/v1/access", "access", []string{"tenantId", "workspaceId", "action", "epoch"}},
		{"POST", "/internal/v1/admissions", "admit", []string{"tenantId", "workspaceId", "action", "ticketId", "kind", "epoch"}},
		{"POST", "/internal/v1/controller-lease/acquire", "lease_acquire", []string{}},
		{"POST", "/internal/v1/controller-lease/renew", "lease_renew", []string{"epoch"}},
		{"POST", "/internal/v1/controller-lease/release", "lease_release", []string{"epoch"}},
		{"POST", "/internal/v1/operations/claim", "claim", []string{"epoch"}},
		{"POST", "/internal/v1/operations/:oid/snapshot", "snapshot", []string{"epoch", "version"}},
		{"POST", "/internal/v1/operations/:oid/effects", "plan", []string{"epoch", "version", "kind", "workspaceId"}},
		{"POST", "/internal/v1/operations/:oid/effects/:eid/result", "effect_result", []string{"epoch", "version", "state", "externalId", "result"}},
		{"POST", "/internal/v1/operations/:oid/advance", "advance", []string{"epoch", "version"}},
		{"POST", "/internal/v1/operations/:oid/defer", "defer", []string{"epoch", "version", "state", "errorCode", "retrySeconds"}},
		{"POST", "/internal/v1/nodes/register", "node_register", []string{"protocolVersion"}},
		{"POST", "/internal/v1/nodes/status", "node_status", []string{"version", "connectionState", "initialized"}},
		{"POST", "/internal/v1/nodes/idle", "node_idle", []string{"version", "admissionEpoch", "idle", "operationId"}},
		{"POST", "/internal/v1/nodes/tickets/:ticket/finish", "node_finish", []string{"version"}},
	}
}

// New injects the store, trust configuration, and logger. No public user CRUD is registered.
func New(store *core.Store, auth *core.Authenticator, log *zap.Logger) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		id := uuid.NewString()
		c.Set("requestId", id)
		c.Header("X-Request-Id", id)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("request panic", zap.String("requestId", id), zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
				failure(c, &core.Fault{Code: "internal_error", Status: 500, Params: core.Object{}})
			}
		}()
		c.Next()
	})
	r.GET("/healthz", func(c *gin.Context) {
		if e := store.Pool.PingContext(c.Request.Context()); e != nil {
			failure(c, &core.Fault{Code: "database_unavailable", Status: 503, Params: core.Object{}})
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
	})
	for _, route := range Routes() {
		r.Handle(route.Method, route.Path, func(c *gin.Context) {
			raw, ok := bearerToken(c.GetHeader("Authorization"))
			if !ok {
				failure(c, &core.Fault{Code: "invalid_service_credential", Status: 401, Params: core.Object{}})
				return
			}
			service, e := auth.Verify(raw, "service")
			if e != nil {
				failure(c, &core.Fault{Code: "invalid_service_credential", Status: 401, Params: core.Object{}})
				return
			}
			public := route.Action == ""
			var user *core.Claims
			if public || route.Action == "access" || route.Action == "admit" {
				if public && service.Role != "gateway" {
					failure(c, &core.Fault{Code: "service_forbidden", Status: 403, Params: core.Object{}})
					return
				}
				user, e = auth.Verify(c.GetHeader("X-Ora-User-Token"), "user")
				if e != nil || user.Caller != service.Subject {
					failure(c, &core.Fault{Code: "invalid_user_credential", Status: 401, Params: core.Object{}})
					return
				}
			}
			body := core.Object{}
			if c.Request.Method != "GET" {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
				decoder := json.NewDecoder(c.Request.Body)
				decoder.UseNumber()
				if e = decoder.Decode(&body); e != nil || body == nil {
					failure(c, &core.Fault{Code: "invalid_json", Status: 400, Params: core.Object{}})
					return
				}
				var extra any
				if decoder.Decode(&extra) != io.EOF {
					failure(c, &core.Fault{Code: "invalid_json", Status: 400, Params: core.Object{}})
					return
				}
				allowed := map[string]bool{}
				for _, f := range route.Fields {
					allowed[f] = true
				}
				for k := range body {
					if !allowed[k] {
						failure(c, &core.Fault{Code: "unknown_field", Status: 400, Params: core.Object{"field": k}})
						return
					}
					if !validField(k, body[k]) {
						failure(c, &core.Fault{Code: "invalid_field_type", Status: 400, Params: core.Object{"field": k}})
						return
					}
				}
				for _, k := range []string{"idle", "initialized"} {
					for _, f := range route.Fields {
						if f == k {
							if _, ok := body[k]; !ok {
								failure(c, &core.Fault{Code: "missing_field", Status: 400, Params: core.Object{"field": k}})
								return
							}
						}
					}
				}
			}
			var out core.Object
			status := 200
			if public {
				limit := 0
				if v := c.Query("limit"); v != "" {
					limit, e = strconv.Atoi(v)
					if e != nil || limit < 1 || limit > 100 {
						failure(c, &core.Fault{Code: "invalid_pagination", Status: 400, Params: core.Object{}})
						return
					}
				}
				out, status, e = store.Public(c.Request.Context(), &core.PublicRequest{Method: c.Request.Method, Path: c.Request.URL.Path, TenantID: c.Param("tid"), ProjectID: c.Param("pid"), WorkspaceID: c.Param("wid"), SpaceID: c.Param("spaceId"), OperationID: c.Param("oid"), CloneID: c.Param("cloneId"), UserID: c.Param("uid"), IssueID: c.Param("iid"), CommentID: c.Param("cid"), LabelID: c.Param("lid"), StatusID: c.Param("sid"), ViewID: c.Param("vid"), RunID: c.Param("rid"), ContextRefID: c.Param("crid"), InteractionID: c.Param("ixid"), FormRef: c.Param("formRef"), Key: c.GetHeader("Idempotency-Key"), Limit: limit, After: c.Query("after"), Query: c.Query("q"), GroupBy: c.Query("by"), Body: body, Identity: user})
			} else {
				out, e = store.Control(c.Request.Context(), &core.ControlRequest{Action: route.Action, OperationID: c.Param("oid"), EffectID: c.Param("eid"), TicketID: c.Param("ticket"), Body: body, Service: service, Identity: user})
			}
			if e != nil {
				f := core.ErrorCode(e)
				if f.Status == 500 {
					log.Error("request failed", zap.String("requestId", c.GetString("requestId")), zap.Error(e))
				}
				failure(c, f)
				return
			}
			c.JSON(status, out)
		})
	}
	r.GET("/api/v1/tenants/:tid/spaces/:spaceId/events", func(c *gin.Context) { sseEvents(store, auth, c) })
	r.NoRoute(func(c *gin.Context) { failure(c, &core.Fault{Code: "not_found", Status: 404, Params: core.Object{}}) })
	return r
}

// sseEvents streams committed space invalidation events to a verified member.
// Authorization reuses the REST path: the caller must be able to read the space.
// Events are lightweight invalidation notices; the authoritative state is always
// fetched over REST afterwards.
func sseEvents(store *core.Store, auth *core.Authenticator, c *gin.Context) {
	_, user, fault := verifyPublicCredentials(c, auth)
	if fault != nil {
		failure(c, fault)
		return
	}
	tid, sid := c.Param("tid"), c.Param("spaceId")
	out, status, e := store.Public(c.Request.Context(), &core.PublicRequest{Method: "GET", Path: "/api/v1/tenants/" + tid + "/spaces/" + sid, TenantID: tid, SpaceID: sid, Identity: user})
	if e != nil {
		failure(c, core.ErrorCode(e))
		return
	}
	if status != 200 || out == nil {
		failure(c, &core.Fault{Code: "not_found", Status: 404, Params: core.Object{}})
		return
	}
	if store.Events == nil {
		failure(c, &core.Fault{Code: "realtime_unavailable", Status: 503, Params: core.Object{}})
		return
	}
	stream, cancel := store.Events.Subscribe(sid)
	defer cancel()
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
	c.Writer.Flush()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case ev := <-stream:
			b, err := json.Marshal(ev)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", b); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// verifyPublicCredentials checks the dual gateway-service and caller-bound
// final-user credentials required by every public endpoint.
func verifyPublicCredentials(c *gin.Context, auth *core.Authenticator) (service, user *core.Claims, fault *core.Fault) {
	raw, ok := bearerToken(c.GetHeader("Authorization"))
	if !ok {
		return nil, nil, &core.Fault{Code: "invalid_service_credential", Status: 401, Params: core.Object{}}
	}
	service, e := auth.Verify(raw, "service")
	if e != nil || service.Role != "gateway" {
		return nil, nil, &core.Fault{Code: "invalid_service_credential", Status: 401, Params: core.Object{}}
	}
	user, e = auth.Verify(c.GetHeader("X-Ora-User-Token"), "user")
	if e != nil || user.Caller != service.Subject {
		return nil, nil, &core.Fault{Code: "invalid_user_credential", Status: 401, Params: core.Object{}}
	}
	return service, user, nil
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func validField(name string, value any) bool {
	switch name {
	case "version", "epoch", "admissionEpoch", "protocolVersion", "retrySeconds", "position":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, e := n.Int64()
		return e == nil
	case "ids":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			if _, ok := v.(string); !ok {
				return false
			}
		}
		return true
	case "targets":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			for k, fv := range m {
				switch k {
				case "type", "id", "task":
					if _, ok := fv.(string); !ok {
						return false
					}
				default:
					return false
				}
			}
		}
		return true
	case "filter", "properties", "input", "values":
		_, ok := value.(map[string]any)
		return ok
	case "targetId":
		_, ok := value.(string)
		return ok
	case "contextRefs":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			for k, fv := range m {
				switch k {
				case "refType", "refId":
					if _, ok := fv.(string); !ok {
						return false
					}
				default:
					return false
				}
			}
		}
		return true
	case "idle", "initialized":
		_, ok := value.(bool)
		return ok
	case "result":
		o, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range o {
			switch k {
			case "jobTerminated", "removed", "terminated", "installed":
				if _, ok := v.(bool); !ok {
					return false
				}
			case "layoutVersion":
				if _, ok := v.(json.Number); !ok {
					return false
				}
			case "commitId", "sandboxInstanceId", "nodeId", "version", "error", "diagnostic":
				if _, ok := v.(string); !ok {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		_, ok := value.(string)
		return ok
	}
}

func failure(c *gin.Context, e *core.Fault) {
	c.AbortWithStatusJSON(e.Status, gin.H{"code": e.Code, "params": e.Params, "requestId": c.GetString("requestId")})
}
