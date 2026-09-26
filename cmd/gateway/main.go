// gateway is the public authentication and reverse-proxy boundary in front of Cloud.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gateway"
	"github.com/wanglongan587/cloud/internal/gateway/devlogin"
	"github.com/wanglongan587/cloud/internal/gateway/github"
	"github.com/wanglongan587/cloud/internal/gateway/idaas"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run() (runErr error) {
	configPath := flag.String("config", "", "configuration file")
	flag.Parse()
	cfg, e := gateway.LoadConfig(*configPath)
	if e != nil {
		return e
	}
	log, e := logger.New(cfg.Logger)
	if e != nil {
		return e
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, e := repository.InitDB(ctx, cfg.Database)
	if e != nil {
		return e
	}
	// The migration gate is shared with Cloud: the Gateway verifies checksums and never runs DDL.
	schema, e := core.NewStore(db)
	if e != nil {
		return e
	}
	defer func() { runErr = errors.Join(runErr, schema.Pool.Close()) }()
	if e := schema.CheckSchema(ctx); e != nil {
		return e
	}
	store, e := gateway.NewStore(schema.Pool)
	if e != nil {
		return e
	}
	var web *gateway.Web
	if cfg.Web.DistDir != "" {
		if web, e = gateway.OpenWeb(cfg.Web.DistDir); e != nil {
			return e
		}
		defer func() { runErr = errors.Join(runErr, web.Close()) }()
	}
	handler, e := buildHandler(cfg, store, web, log)
	if e != nil {
		return e
	}
	gin.SetMode(cfg.Server.Mode)
	server := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Server.Port), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout, IdleTimeout: 60 * time.Second}
	cleanupCtx, stopCleanup := context.WithCancel(ctx)
	var cleanup sync.WaitGroup
	cleanup.Go(func() {
		gateway.RunCleanup(cleanupCtx, store, cfg.Session.CleanupInterval, cfg.Session.Retention, cfg.Session.CleanupBatch, log)
	})
	defer func() {
		stopCleanup()
		cleanup.Wait()
	}()
	failed := make(chan error, 1)
	go func() {
		log.Info("Gateway listening", zap.String("address", server.Addr), zap.String("publicOrigin", cfg.Public.BaseURL), zap.String("upstream", cfg.Cloud.Upstream))
		failed <- server.ListenAndServe()
	}()
	select {
	case e = <-failed:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return server.Shutdown(shutdown)
	}
}

// buildHandler loads key material and assembles the authentication boundary. Secrets are read from
// files into process memory only and are never logged. A nil web serves no frontend.
func buildHandler(cfg *gateway.Config, store *gateway.Store, web *gateway.Web, log *zap.Logger) (http.Handler, error) {
	origin, e := gateway.PublicOrigin(cfg.Public.BaseURL, cfg.Public.Development)
	if e != nil {
		return nil, e
	}
	serviceKey, e := gateway.LoadPrivateKey(cfg.Tokens.ServicePrivateKeyFile)
	if e != nil {
		return nil, e
	}
	userKey, e := gateway.LoadPrivateKey(cfg.Tokens.UserPrivateKeyFile)
	if e != nil {
		return nil, e
	}
	issuer, e := gateway.NewIssuer(cfg.Tokens.Issuer, cfg.Tokens.Audience, cfg.Tokens.ServiceSubject, gateway.SigningKey{ID: cfg.Tokens.ServiceKeyID, Key: serviceKey}, gateway.SigningKey{ID: cfg.Tokens.UserKeyID, Key: userKey}, cfg.Tokens.Lifetime, time.Now)
	if e != nil {
		return nil, e
	}
	pkceKey, e := os.ReadFile(cfg.Login.PKCEKeyFile)
	if e != nil {
		return nil, fmt.Errorf("read PKCE key: %w", e)
	}
	// Config validation guarantees a usable set: the external provider named by login.provider
	// (only its secret is read) and, on a loopback development origin only, the "dev" adapter.
	providers := map[string]gateway.Authenticator{}
	if cfg.Login.Provider != "" {
		if providers[cfg.Login.Provider], e = buildProvider(cfg); e != nil {
			return nil, e
		}
	}
	var dev *devlogin.Authenticator
	if cfg.Login.DevelopmentProvider {
		if dev, e = devlogin.New(origin, time.Now); e != nil {
			return nil, e
		}
		providers[devlogin.Name] = dev
		log.Warn("development login provider enabled: any typed identity signs in", zap.String("publicOrigin", origin))
	}
	login, e := gateway.NewLogin(store, providers, cfg.Login.Provider, pkceKey, origin+gateway.CallbackPath, cfg.Login.AttemptTTL, cfg.Session.TTL)
	if e != nil {
		return nil, e
	}
	upstream, e := url.Parse(cfg.Cloud.Upstream)
	if e != nil {
		return nil, fmt.Errorf("parse cloud upstream: %w", e)
	}
	engine, e := gateway.NewHandler(&gateway.Options{
		Store:           store,
		Login:           login,
		Issuer:          issuer,
		Limiter:         gateway.NewRateLimiter(cfg.Login.RateLimitPerMinute, cfg.Login.RateLimitBurst, 100000, time.Now),
		Upstream:        upstream,
		UpstreamTimeout: cfg.Cloud.Timeout,
		PublicOrigin:    origin,
		Cookies:         gateway.CookiePolicy{Secure: strings.HasPrefix(origin, "https://"), CallbackPath: gateway.CallbackPath},
		Log:             log,
		Now:             time.Now,
		Web:             web,
	})
	if e != nil {
		return nil, e
	}
	if dev != nil {
		dev.Routes(engine)
	}
	return engine, nil
}

func buildProvider(cfg *gateway.Config) (gateway.Authenticator, error) {
	switch cfg.Login.Provider {
	case gateway.ProviderGitHub:
		secret, e := readSecret(cfg.GitHub.ClientSecretFile, "github client secret")
		if e != nil {
			return nil, e
		}
		return github.New(&github.Options{ClientID: cfg.GitHub.ClientID, ClientSecret: secret, AuthorizeURL: cfg.GitHub.AuthorizeURL, TokenURL: cfg.GitHub.TokenURL, UserURL: cfg.GitHub.UserURL, Source: cfg.GitHub.Source, HTTP: github.NewHTTPClient(cfg.Cloud.Timeout)})
	case gateway.ProviderHuaweiIDaaS:
		secret, e := readSecret(cfg.IDaaS.ClientSecretFile, "IDaaS client secret")
		if e != nil {
			return nil, e
		}
		return idaas.New(&idaas.Options{BaseURL: cfg.IDaaS.BaseURL, ClientID: cfg.IDaaS.ClientID, ClientSecret: secret, DisplayNameField: cfg.IDaaS.DisplayNameField, HTTP: idaas.NewHTTPClient(cfg.IDaaS.Timeout)})
	default:
		return nil, fmt.Errorf("unsupported login provider %q", cfg.Login.Provider)
	}
}

func readSecret(path, name string) (string, error) {
	value, e := os.ReadFile(path)
	if e != nil {
		return "", fmt.Errorf("read %s: %w", name, e)
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	return secret, nil
}
