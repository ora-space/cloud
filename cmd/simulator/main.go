// simulator runs the phase-one cloud flow over real HTTP/PostgreSQL and a disk-backed Git simulator.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repository"
	"github.com/wanglongan587/cloud/internal/simulator"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run() error {
	file := flag.String("config", "", "configuration file for a dedicated migrated simulation database")
	root := flag.String("root", ".local/demo", "durable simulator storage")
	flag.Parse()
	cfg, e := config.Load(*file)
	if e != nil {
		return e
	}
	db, e := repository.InitDB(context.Background(), cfg.Database)
	if e != nil {
		return e
	}
	store, e := core.NewStore(db)
	if e != nil {
		return e
	}
	defer store.Pool.Close()
	ctx := context.Background()
	if e := store.CheckSchema(ctx); e != nil {
		return e
	}
	absolute, e := filepath.Abs(*root)
	if e != nil {
		return e
	}
	repo := filepath.Join(absolute, "fixture")
	if e := os.MkdirAll(repo, 0o700); e != nil {
		return e
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); os.IsNotExist(err) {
		for _, args := range [][]string{{"init", "--initial-branch=main", repo}} {
			if e := git(args...); e != nil {
				return e
			}
		}
		if e := os.WriteFile(filepath.Join(repo, "README.md"), []byte("Ora phase-one real Git fixture\n"), 0o600); e != nil {
			return e
		}
		if e := git("-C", repo, "add", "."); e != nil {
			return e
		}
		if e := git("-C", repo, "-c", "user.name=Ora Simulator", "-c", "user.email=simulator@example.invalid", "commit", "-m", "fixture"); e != nil {
			return e
		}
	}
	credentials, e := simulator.NewCredentials()
	if e != nil {
		return e
	}
	auth, e := core.NewAuthenticator("ora-cloud", credentials.Trust)
	if e != nil {
		return e
	}
	gin.SetMode(gin.ReleaseMode)
	cloud := httptest.NewServer(router.New(store, auth, zap.NewNop()))
	defer cloud.Close()
	substrate, e := simulator.NewSubstrate(filepath.Join(absolute, "substrate"), map[string]string{"https://example.invalid/repo.git": repo})
	if e != nil {
		return e
	}
	external := httptest.NewServer(substrate)
	defer external.Close()
	subject := "demo-" + uuid.NewString()
	tenant, e := store.Bootstrap(ctx, "Phase one demo", "simulator", subject, "Demo user")
	if e != nil {
		return e
	}
	client := &simulator.Client{URL: cloud.URL, Credentials: credentials, HTTP: &http.Client{Timeout: 30 * time.Second}, Subject: "controller-" + uuid.NewString()}
	controller := &simulator.Controller{Client: client, SubstrateURL: external.URL}
	if e := controller.Acquire(ctx); e != nil {
		return e
	}
	user := &core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subject}, Source: "simulator"}
	gateway := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "simulator-gateway"}}
	created, status, e := client.Call(ctx, "POST", "/api/v1/tenants/"+tenant.S("tenantId")+"/projects", "gateway", gateway, user, "demo-create", core.Object{"name": "Demo project", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"})
	if e != nil || status != 202 {
		return fmt.Errorf("create: %d %v %v", status, e, created)
	}
	if e := controller.Drain(ctx); e != nil {
		return e
	}
	workspace, status, e := client.Call(ctx, "GET", "/api/v1/tenants/"+tenant.S("tenantId")+"/workspaces/"+created.O("workspace").S("id"), "gateway", gateway, user, "", nil)
	if e != nil || status != 200 {
		return fmt.Errorf("workspace: %d %v", status, e)
	}
	if _, e = client.Control(ctx, "/internal/v1/controller-lease/release", core.Object{"epoch": controller.Epoch}); e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(core.Object{"phase": "cloud core + simulated execution", "tenantId": tenant.S("tenantId"), "projectId": created.O("resource").S("id"), "workspace": workspace, "storageRoot": substrate.Root})
}

func git(args ...string) error {
	b, e := exec.Command("git", args...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("git: %w: %s", e, b)
	}
	return nil
}
