// Command repo-executor runs the Repo Executor: the only Ora process that contacts external Git
// repositories. It serves Cloud's task listener over mTLS and holds no state; on shutdown it cancels
// in-flight tasks, which still destroy their task directories and credential material before their
// responses are written.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialstore"
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

func run() (runErr error) {
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
	egress, err := repoexec.NewEgress(net.DefaultResolver, cfg.EgressAllow, cfg.internalHosts())
	if err != nil {
		return err
	}
	executor, err := repoexec.NewExecutor(&repoexec.Config{
		PublicKeys: keys,
		Redeemer: repoexec.RedeemFunc(func(ctx context.Context, authorization string) (repoexec.Credential, error) {
			c, err := store.Redeem(ctx, authorization)
			return repoexec.Credential{Username: c.Username, Token: c.Token, Version: c.Version}, err
		}),
		Egress:       egress,
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
	return serve(cfg.Addr, serverTLS, server.Handler())
}

// serve runs the listener until SIGINT/SIGTERM. Every request context derives from tasks, so
// canceling it ends in-flight Git processes; Shutdown then waits for those handlers, whose tasks
// clean up and answer, within shutdownGrace.
func serve(addr string, tlsConfig *tls.Config, handler http.Handler) error {
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tasks, cancelTasks := context.WithCancel(context.Background())
	defer cancelTasks()
	// No read/write timeouts: a task's response is written when the task ends, bounded by its own
	// timeout.
	srv := &http.Server{
		Addr: addr, Handler: handler, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return tasks },
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServeTLS("", "") }()
	select {
	case err := <-failed:
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
	if served := <-failed; !errors.Is(served, http.ErrServerClosed) {
		err = errors.Join(err, served)
	}
	return err
}
