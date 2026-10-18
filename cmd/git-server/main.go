// Command git-server serves Project repositories over smart HTTP to sandboxes and, on a separate
// listener, to the Repo Executor, and answers Cloud's read-only ref query. Git also runs it as the
// pre-receive hook (-pre-receive).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/wanglongan587/cloud/internal/gitserver"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run owns the three listeners and the hook channel; shutdown drains HTTP before closing the channel
// that in-flight pushes still need for their pre-receive decision.
func run() (runErr error) {
	path := flag.String("config", "", "Cloud database configuration")
	health := flag.Bool("healthcheck", false, "verify readiness through the ref listener's health route")
	preReceive := flag.Bool("pre-receive", false, "run as the pre-receive hook (invoked by git)")
	flag.Parse()
	if *preReceive {
		// The hook inherits receive-pack's environment, which already fixes the git binary on PATH.
		return gitserver.RunPreReceive(os.Stdin, os.Stderr, "git")
	}
	deployment, err := gitserver.LoadConfig()
	if err != nil {
		return err
	}
	if *health {
		return checkHealth(&deployment)
	}
	cfg, err := config.Load(*path)
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
	log, err := logger.New(cfg.Logger)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	cloudClient, err := deployment.CloudClient()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	service, err := gitserver.New(&gitserver.Options{Store: store, RepositoryRoot: deployment.RepositoryRoot, GitProgram: deployment.GitProgram, CloudClient: cloudClient, Hook: []string{self, "-pre-receive"}, Health: store.Pool.PingContext, Logger: log})
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, service.Close()) }()
	repositoryTLS, err := deployment.RepositoryTLS()
	if err != nil {
		return err
	}
	serviceTLS, err := deployment.ServiceRepositoryTLS()
	if err != nil {
		return err
	}
	refTLS, err := deployment.RefTLS()
	if err != nil {
		return err
	}
	servers := []*http.Server{
		{Addr: deployment.RepositoryAddress, Handler: http.HandlerFunc(service.RepositoryHandler), TLSConfig: repositoryTLS},
		{Addr: deployment.ServiceRepositoryAddress, Handler: http.HandlerFunc(service.ServiceRepositoryHandler), TLSConfig: serviceTLS},
		{Addr: deployment.RefAddress, Handler: http.HandlerFunc(service.RefHandler), TLSConfig: refTLS},
	}
	for _, server := range servers {
		// No write timeout: pack transfers are bounded by the client and by request cancellation.
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

// checkHealth calls the ref listener's health route, verifying the server identity
// against the deployment CA; it reads no private key.
func checkHealth(deployment *gitserver.Config) error {
	trust, err := os.ReadFile(deployment.SandboxCAFile)
	if err != nil {
		return errors.New("git health trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return errors.New("invalid git health trust")
	}
	// The ref listener may be bound to one network's address, so dial its configured host.
	host, port, err := net.SplitHostPort(deployment.RefAddress)
	if err != nil {
		return errors.New("invalid git health address")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: deployment.HealthServerName}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("git health redirect forbidden") }}
	response, err := client.Get("https://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return errors.New("git server not ready")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("git server not ready")
	}
	return nil
}
