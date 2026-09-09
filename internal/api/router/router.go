// Package router binds typed routes to the cloud core and verifies two independent credentials.
package router

import (
	"encoding/json"
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
		{"GET", "/api/v1/tenants/:tid/members", "", nil},
		{"PUT", "/api/v1/tenants/:tid/members/:uid", "", []string{"role", "status", "version"}},
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
		{"GET", "/api/v1/tenants/:tid/operations/:oid", "", nil},
		{"POST", "/api/v1/tenants/:tid/operations/:oid/retry", "", []string{"version"}},
		{"GET", "/api/v1/tenants/:tid/resource-status", "", nil},
		{"POST", "/api/v1/tenants/:tid/workspaces/:wid/administrative-stop", "", []string{"version"}},
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
				out, status, e = store.Public(c.Request.Context(), &core.PublicRequest{Method: c.Request.Method, Path: c.Request.URL.Path, TenantID: c.Param("tid"), ProjectID: c.Param("pid"), WorkspaceID: c.Param("wid"), OperationID: c.Param("oid"), UserID: c.Param("uid"), Key: c.GetHeader("Idempotency-Key"), Limit: limit, After: c.Query("after"), Body: body, Identity: user})
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
	r.NoRoute(func(c *gin.Context) { failure(c, &core.Fault{Code: "not_found", Status: 404, Params: core.Object{}}) })
	return r
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
	case "version", "epoch", "admissionEpoch", "protocolVersion", "retrySeconds":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, e := n.Int64()
		return e == nil
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
			case "jobTerminated", "removed", "terminated":
				if _, ok := v.(bool); !ok {
					return false
				}
			case "layoutVersion":
				if _, ok := v.(json.Number); !ok {
					return false
				}
			case "commitId", "sandboxInstanceId", "nodeId":
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
