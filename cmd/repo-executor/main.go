// Command repo-executor runs the Repo Executor: the only Ora process that contacts external Git
// repositories. It serves Cloud's task listener over mTLS and holds no state; on shutdown it cancels
// in-flight tasks, which still destroy their task directories and credential material before their
// responses are written.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/egress"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

// shutdownGrace bounds how long shutdown waits for cancelled tasks to clean up and answer.
const shutdownGrace = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// deploymentEgressPath prefers the executor-scoped variable over the shared one, so a deployment can
// point this process at a different file without moving Cloud and model-gateway. The flag still wins
// over both.
func deploymentEgressPath(lookup func(string) string) string {
	if path := strings.TrimSpace(lookup("REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE")); path != "" {
		return path
	}
	return strings.TrimSpace(lookup("ORA_DEPLOYMENT_EGRESS_FILE"))
}

func run() (runErr error) {
	egressPath := flag.String("deployment-egress", deploymentEgressPath(os.Getenv), "shared deployment egress JSON file")
	checkEgress := flag.Bool("check-egress", false, "validate the deployment egress file and print its identity")
	flag.Parse()
	// A relative path would resolve against the container's working directory, which differs between
	// the service image and the deployment tool; the shared file is always an absolute mount point.
	if *egressPath != "" && !filepath.IsAbs(*egressPath) {
		return errors.New("deployment egress path must be absolute")
	}
	// The same parser every egress caller uses: a candidate file is accepted by the deployment tool
	// only if this process could start with it. It needs no keys, database or sockets.
	policy, err := egress.Load(*egressPath)
	if err != nil {
		return err
	}
	dns := egress.DeploymentDNSConfig()
	if *checkEgress {
		return json.NewEncoder(os.Stdout).Encode(struct {
			SchemaVersion int    `json:"schemaVersion"`
			Digest        string `json:"digest"`
		}{1, policy.Digest(dns)})
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	log, err := logger.New(logger.Config{Level: cfg.LogLevel, EnableConsole: true})
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	keys, err := cloneauth.LoadPublicKeys(cfg.AuthorizationPublicKeysFile)
	if err != nil {
		return err
	}
	storeTLS, err := repoexec.ClientTLS(cfg.CertFile, cfg.KeyFile, cfg.CredentialStoreCA)
	if err != nil {
		return err
	}
	store, err := credentialstore.NewRedemptionClient(&credentialstore.ClientConfig{BaseURL: cfg.CredentialStoreURL, TLS: storeTLS})
	if err != nil {
		return err
	}
	external, err := repoexec.NewEgress(&repoexec.EgressConfig{
		Policy: policy, DNS: dns,
		Development: cfg.Development, DevelopmentHosts: cfg.DevelopmentHosts,
		InternalHosts: cfg.internalHosts(),
	})
	if err != nil {
		return err
	}
	defer external.Close()
	executor, err := repoexec.NewExecutor(&repoexec.Config{
		PublicKeys: keys,
		Redeemer: repoexec.RedeemFunc(func(ctx context.Context, authorization string) (repoexec.Credential, error) {
			c, err := store.Redeem(ctx, authorization)
			return repoexec.Credential{Username: c.Username, Token: c.Token, Version: c.Version}, err
		}),
		Egress:       external,
		GitServerURL: cfg.GitServerURL, GitServerCAFile: cfg.GitServerCAFile, CertFile: cfg.CertFile, KeyFile: cfg.KeyFile,
		ExternalCAFile: cfg.ExternalCAFile, WorkRoot: cfg.WorkRoot, HelperPath: cfg.HelperPath, GitProgram: cfg.GitProgram,
		MaxTaskBytes: cfg.MaxTaskBytes,
		Limits:       repoexec.Limits{Sync: cfg.SyncTimeoutMax, Publish: cfg.PublishTimeoutMax, Probe: cfg.ProbeTimeoutMax},
		Logger:       log,
	})
	if err != nil {
		return err
	}
	server, err := repoexec.NewServer(executor, cfg.Slots, log)
	if err != nil {
		return err
	}
	serverTLS, err := repoexec.ServerTLS(cfg.CertFile, cfg.KeyFile, cfg.ClientCAFile)
	if err != nil {
		return err
	}
	health := egress.HealthMetadata(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), policy.Digest(dns))
	return serve(&cfg, serverTLS, server.Handler(), health)
}

// serve runs the task listener and the health listener until SIGINT/SIGTERM. Every request context
// derives from tasks, so canceling it ends in-flight Git processes; Shutdown then waits for those
// handlers, whose tasks clean up and answer, within shutdownGrace.
func serve(cfg *config, tlsConfig *tls.Config, handler, health http.Handler) error {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tasks, cancelTasks := context.WithCancel(context.Background())
	defer cancelTasks()
	// No read/write timeouts on the task listener: a response is written when the task ends, bounded
	// by its own timeout. The health listener serves fixed headers and no task data.
	srv := &http.Server{
		Addr: cfg.Addr, Handler: handler, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return tasks },
	}
	healthSrv := &http.Server{Addr: cfg.HealthAddr, Handler: health, ReadHeaderTimeout: 5 * time.Second}
	failed := make(chan error, 2)
	go func() { failed <- srv.ListenAndServeTLS("", "") }()
	go func() { failed <- healthSrv.ListenAndServe() }()
	select {
	case err := <-failed:
		cancelTasks()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = healthSrv.Shutdown(shutdown)
		_ = srv.Shutdown(shutdown)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals.Done():
	}
	cancelTasks()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutdown)
	err = errors.Join(err, healthSrv.Shutdown(shutdown))
	served := <-failed
	if !errors.Is(served, http.ErrServerClosed) {
		err = errors.Join(err, served)
	}
	return err
}
