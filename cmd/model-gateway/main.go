// Command model-gateway serves personal credential writes, scoped model leases and streaming model requests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/modelgateway"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run owns every listener and request context; shutdown cancels model streams before joining servers.
func run() (runErr error) {
	path := flag.String("config", "", "Cloud database and verification-key configuration")
	health := flag.Bool("healthcheck", false, "verify credential service readiness without reading private keys")
	flag.Parse()
	if *health {
		deployment, err := modelgateway.LoadConfig()
		if err != nil {
			return err
		}
		return modelgateway.CheckHealth(&deployment)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	deployment, err := modelgateway.LoadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := repository.InitDB(ctx, cfg.Database)
	if err != nil {
		return err
	}
	store, err := core.NewStore(db)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.CheckSchema(ctx); err != nil {
		return err
	}
	auth, err := core.NewAuthenticator(cfg.Auth.Audience, cfg.Auth.Keys)
	if err != nil {
		return err
	}
	cipher, err := modelgateway.LoadCipher(deployment.MasterKeyFile, deployment.MasterKeyID)
	if err != nil {
		return err
	}
	if err := modelgateway.VerifyExistingKey(ctx, store.Pool, cipher); err != nil {
		return err
	}
	log, err := logger.New(cfg.Logger)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	client, err := modelgateway.NewUpstreamClient(modelgateway.UpstreamConfig{DevelopmentHosts: deployment.DevelopmentHosts, DNS: deployment.DNS})
	if err != nil {
		return err
	}
	service, err := modelgateway.New(&modelgateway.Options{Store: store, Auth: auth, Cipher: cipher, PublicOrigin: deployment.PublicOrigin, Upstream: client, Health: store.Pool.PingContext, Logger: log})
	if err != nil {
		return err
	}
	defer service.Close()
	serverTLS, err := deployment.ServerTLS()
	if err != nil {
		return err
	}
	grantTLS, err := deployment.GrantTLS()
	if err != nil {
		return err
	}
	servers := []*http.Server{
		{Addr: deployment.CredentialAddress, Handler: http.HandlerFunc(service.CredentialHandler), TLSConfig: serverTLS},
		{Addr: deployment.RuntimeAddress, Handler: http.HandlerFunc(service.ModelHandler), TLSConfig: serverTLS.Clone()},
		{Addr: deployment.GrantAddress, Handler: http.HandlerFunc(service.GrantHandler), TLSConfig: grantTLS},
	}
	for _, server := range servers {
		server.ReadHeaderTimeout = 5 * time.Second
		server.IdleTimeout = time.Minute
		server.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	failed := make(chan error, len(servers))
	for _, server := range servers {
		go func() { failed <- server.ListenAndServeTLS("", "") }()
	}
	waits := len(servers)
	select {
	case err = <-failed:
		waits--
	case <-ctx.Done():
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for _, server := range servers {
		err = errors.Join(err, server.Shutdown(shutdown))
	}
	for range waits {
		select {
		case <-failed:
		case <-shutdown.Done():
			return errors.Join(err, shutdown.Err())
		}
	}
	return err
}
