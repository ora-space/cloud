package simulator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// Credentials are generated only for the isolated simulator/test process.
type Credentials struct {
	Private map[string]ed25519.PrivateKey
	Trust   []core.TrustedKey
}

func NewCredentials() (*Credentials, error) {
	c := &Credentials{Private: map[string]ed25519.PrivateKey{}}
	for _, role := range []string{"gateway", "controller", "node", "user"} {
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		kind := "service"
		if role == "user" {
			kind = "user"
		}
		c.Private[role] = key
		c.Trust = append(c.Trust, core.TrustedKey{ID: role, Issuer: "ora-simulator", Kind: kind, Role: role, Key: pub})
	}
	return c, nil
}

// Token signs short-lived claims for one known simulator role.
//
//nolint:gocritic // Copy before adding timestamps; signing concurrent requests must not mutate shared identity claims.
func (c *Credentials) Token(role string, claims core.Claims) (string, error) {
	key, ok := c.Private[role]
	if !ok {
		return "", fmt.Errorf("unknown simulator credential role %q", role)
	}
	now := time.Now()
	claims.RegisteredClaims = jwt.RegisteredClaims{Issuer: "ora-simulator", Subject: claims.Subject, Audience: jwt.ClaimStrings{"ora-cloud"}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}
	claims.Kind = "service"
	claims.Role = role
	if role == "user" {
		claims.Kind = "user"
		claims.Role = ""
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	t.Header["kid"] = role
	raw, err := t.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign %s simulator credential: %w", role, err)
	}
	return raw, nil
}

// Client calls cloud through actual HTTP, issuing short-lived internal simulator credentials.
type Client struct {
	URL         string
	Credentials *Credentials
	HTTP        *http.Client
	Subject     string
}

//nolint:gocritic // Value claims intentionally isolate each signed request from concurrent callers.
func (c *Client) Call(ctx context.Context, method, path, role string, claims core.Claims, user *core.Claims, key string, body core.Object) (core.Object, int, error) {
	raw := jsonString(body)
	req, e := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewBufferString(raw))
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	token, e := c.Credentials.Token(role, claims)
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if user != nil {
		u := *user
		u.Caller = claims.Subject
		userToken, tokenErr := c.Credentials.Token("user", u)
		if tokenErr != nil {
			return nil, 0, tokenErr
		}
		req.Header.Set("X-Ora-User-Token", userToken)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, e := c.HTTP.Do(req)
	if e != nil {
		return nil, 0, e
	}
	defer res.Body.Close()
	out := core.Object{}
	e = json.NewDecoder(res.Body).Decode(&out)
	if e != nil {
		return nil, res.StatusCode, e
	}
	return out, res.StatusCode, nil
}

func (c *Client) Control(ctx context.Context, path string, body core.Object) (core.Object, error) {
	out, status, e := c.Call(ctx, "POST", path, "controller", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: c.Subject}}, nil, "", body)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, fmt.Errorf("cloud %s: %d %s", path, status, jsonString(out))
	}
	return out, nil
}

// Controller has no database handle; recovery always queries external effect IDs before dispatch.
type Controller struct {
	Client       *Client
	SubstrateURL string
	Epoch        int64
	Operation    core.Object
}

func (c *Controller) Acquire(ctx context.Context) error {
	o, e := c.Client.Control(ctx, "/internal/v1/controller-lease/acquire", core.Object{})
	if e == nil {
		c.Epoch = o.N("epoch")
	}
	return e
}

func (c *Controller) command(ctx context.Context, suffix string, b core.Object) (core.Object, error) {
	b["epoch"], b["version"] = c.Epoch, c.Operation.N("version")
	return c.Client.Control(ctx, "/internal/v1/operations/"+c.Operation.S("id")+suffix, b)
}

func (c *Controller) external(ctx context.Context, method, id string, b core.Object) (core.Object, int, error) {
	req, e := http.NewRequestWithContext(ctx, method, c.SubstrateURL+"/effects/"+id, bytes.NewBufferString(jsonString(b)))
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.Client.HTTP.Do(req)
	if e != nil {
		return nil, 0, e
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, 404, nil
	}
	data, e := io.ReadAll(resp.Body)
	if e != nil {
		return nil, resp.StatusCode, e
	}
	out := core.Object{}
	if len(data) > 0 {
		e = json.Unmarshal(data, &out)
	}
	return out, resp.StatusCode, e
}

func objects(o core.Object, k string) ([]core.Object, error) {
	var out []core.Object
	switch a := o[k].(type) {
	case nil:
		return nil, nil
	case []any:
		for i, v := range a {
			value, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cloud field %q item %d is not an object", k, i)
			}
			out = append(out, core.Object(value))
		}
	case []core.Object:
		out = a
	default:
		return nil, fmt.Errorf("cloud field %q is not an array", k)
	}
	return out, nil
}

func (c *Controller) submit(ctx context.Context, effect, external core.Object) error {
	b := core.Object{"state": external.S("state"), "externalId": external.S("externalId"), "result": external.O("result")}
	out, e := c.command(ctx, "/effects/"+effect.S("id")+"/result", b)
	if e == nil {
		c.Operation = out.O("operation")
	}
	return e
}

func (c *Controller) deferExternalFailure(ctx context.Context, kind string, status int, external core.Object, cause error) error {
	state, code := "retry_wait", "external_failure"
	if cause != nil || status == http.StatusGatewayTimeout || external.S("state") == "running" {
		code = "substrate_timeout"
	}
	if kind == "worktree_delete" && cause == nil {
		code = "git_cleanup_failed"
	}
	if kind == "sandbox_terminate" && external.S("state") == "running" {
		state, code = "blocked", "termination_unconfirmed"
	}
	_, deferErr := c.command(ctx, "/defer", core.Object{"state": state, "errorCode": code, "retrySeconds": 1})
	if deferErr == nil {
		c.Operation = nil
	}
	if cause != nil {
		return errors.Join(fmt.Errorf("substrate %s request: %w", kind, cause), deferErr)
	}
	return errors.Join(fmt.Errorf("substrate %s: HTTP %d; stable effect remains tracked: %s", kind, status, external.S("diagnostic")), deferErr)
}

// Step makes one bounded transition; callers can stop/restart between every persisted stage.
func (c *Controller) Step(ctx context.Context) (bool, error) {
	if _, e := c.Client.Control(ctx, "/internal/v1/controller-lease/renew", core.Object{"epoch": c.Epoch}); e != nil {
		return false, e
	}
	var snap core.Object
	var err error
	if c.Operation == nil {
		snap, err = c.Client.Control(ctx, "/internal/v1/operations/claim", core.Object{"epoch": c.Epoch})
		if err != nil {
			return false, err
		}
		if snap["operation"] == nil {
			return true, nil
		}
		c.Operation = snap.O("operation")
	} else {
		snap, err = c.command(ctx, "/snapshot", core.Object{})
		if err != nil {
			return false, err
		}
	}
	effects, err := objects(snap, "effects")
	if err != nil {
		return false, err
	}
	workspaces, err := objects(snap, "workspaces")
	if err != nil {
		return false, err
	}
	sandboxes, err := objects(snap, "sandboxes")
	if err != nil {
		return false, err
	}
	nodes, err := objects(snap, "nodes")
	if err != nil {
		return false, err
	}
	for _, effect := range effects {
		if effect.N("reconciledEpoch") == c.Epoch {
			continue
		}
		external, status, e := c.external(ctx, "GET", effect.S("id"), nil)
		if e != nil {
			return false, c.deferExternalFailure(ctx, effect.S("kind"), status, external, e)
		}
		if status == 404 {
			external = core.Object{"state": "absent", "result": core.Object{}}
		}
		if e := c.submit(ctx, effect, external); e != nil {
			return false, e
		}
	}
	step := c.Operation.S("step")
	switch step {
	case "node":
		for _, sandbox := range sandboxes {
			if sandbox.S("workspaceId") != c.Operation.S("workspaceId") || sandbox["terminatedAt"] != nil {
				continue
			}
			effect := core.Object{}
			for _, candidate := range effects {
				if candidate.S("kind") == "sandbox_ensure" {
					effect = candidate
				}
			}
			nodeID := effect.O("result").S("nodeId")
			if nodeID == "" {
				return false, fmt.Errorf("simulator node id missing")
			}
			claims := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: nodeID}, WorkspaceID: sandbox.S("workspaceId"), SandboxID: sandbox.S("id"), Generation: sandbox.N("generation")}
			n, status, e := c.Client.Call(ctx, "POST", "/internal/v1/nodes/register", "node", claims, nil, "", core.Object{"protocolVersion": 1})
			if e != nil || status != 200 {
				return false, fmt.Errorf("node registration: %d %v %s", status, e, jsonString(n))
			}
			n, status, e = c.Client.Call(ctx, "POST", "/internal/v1/nodes/status", "node", claims, nil, "", core.Object{"version": n.N("version"), "connectionState": "connected", "initialized": true})
			if e != nil || status != 200 {
				return false, fmt.Errorf("node status: %d %v %s", status, e, jsonString(n))
			}
		}
	case "quiesce":
		for _, w := range workspaces {
			if c.Operation.S("workspaceId") != "" && w.S("id") != c.Operation.S("workspaceId") {
				continue
			}
			for _, sandbox := range sandboxes {
				if sandbox.S("workspaceId") != w.S("id") || sandbox["terminatedAt"] != nil {
					continue
				}
				for _, n := range nodes {
					if n.S("sandboxInstanceId") != sandbox.S("id") || n["endedAt"] != nil {
						continue
					}
					claims := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: n.S("id")}, WorkspaceID: w.S("id"), SandboxID: sandbox.S("id"), Generation: sandbox.N("generation")}
					out, status, e := c.Client.Call(ctx, "POST", "/internal/v1/nodes/idle", "node", claims, nil, "", core.Object{"version": n.N("version"), "admissionEpoch": w.N("admissionEpoch"), "operationId": c.Operation.S("id"), "idle": true})
					if e != nil || status != 200 {
						return false, fmt.Errorf("node idle: %d %v %s", status, e, jsonString(out))
					}
				}
			}
		}
	default:
		kinds := map[string]string{"storage": "storage_ensure", "worktree": "worktree_ensure", "sandbox": "sandbox_ensure", "terminate": "sandbox_terminate", "cleanup": "worktree_delete", "storage_delete": "storage_delete"}
		kind := kinds[step]
		if kind == "" {
			return false, fmt.Errorf("unknown step %s", step)
		}
		targets := []core.Object{{}}
		if step != "storage" && step != "storage_delete" {
			targets = nil
			for _, w := range workspaces {
				if c.Operation.S("workspaceId") == "" || c.Operation.S("workspaceId") == w.S("id") {
					targets = append(targets, w)
				}
			}
		}
		for _, w := range targets {
			var sandbox core.Object
			if step == "terminate" {
				for _, v := range sandboxes {
					if v.S("workspaceId") == w.S("id") && v["terminatedAt"] == nil {
						sandbox = v
					}
				}
				if sandbox == nil {
					continue
				}
			}
			planned, e := c.command(ctx, "/effects", core.Object{"kind": kind, "workspaceId": w.S("id")})
			if e != nil {
				return false, e
			}
			c.Operation = planned.O("operation")
			effect := planned.O("effect")
			external, status, e := c.external(ctx, "GET", effect.S("id"), nil)
			if e != nil {
				return false, c.deferExternalFailure(ctx, kind, status, external, e)
			}
			if status == 404 || external.S("state") != "succeeded" {
				if _, e = c.command(ctx, "/snapshot", core.Object{}); e != nil {
					return false, e
				}
				external, status, e = c.external(ctx, "PUT", effect.S("id"), effect.O("request"))
				if e != nil {
					return false, c.deferExternalFailure(ctx, kind, status, external, e)
				}
				if status != 200 {
					return false, c.deferExternalFailure(ctx, kind, status, external, nil)
				}
			}
			if e := c.submit(ctx, effect, external); e != nil {
				return false, e
			}
		}
	}
	out, e := c.command(ctx, "/advance", core.Object{})
	if e != nil {
		return false, e
	}
	c.Operation = out
	if out.S("state") == "succeeded" {
		c.Operation = nil
	}
	return false, nil
}

func (c *Controller) Drain(ctx context.Context) error {
	for i := 0; i < 100; i++ {
		done, e := c.Step(ctx)
		if e != nil {
			return e
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("simulation exceeded transition bound")
}
