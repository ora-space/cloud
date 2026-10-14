// Command credential-store runs the independent Credential Service.
//
// It is a separate process, on its own volume, with its own key material and its own trust root
// (identity-access 20261010-user-managed-git-secret-store D1). Nothing it holds is reachable from
// Cloud except through the mutually authenticated ingress, and it shares no database, connection,
// volume or credential with Cloud's business store.
//
// The command is deliberately thin. Every decision it could get wrong lives in internal/credentialstore
// and internal/config: the startup gate that refuses to run without a usable key-encryption key, the
// identity table that fixes which certificate may do what, and the ordering of the store operations.
// What is left here is the deployment's own configuration and the process lifecycle.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/logger"
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
	cfg, e := config.LoadCredentialStoreConfig(*configPath)
	if e != nil {
		return e
	}
	// D7 makes every operation an audit record, so a log destination is part of the contract rather
	// than an operational preference: a process that cannot write the record must not accept the
	// operation. An unconfigured logger would otherwise come up healthy and discard all of them.
	if !cfg.Logger.EnableConsole && strings.TrimSpace(cfg.Logger.Filename) == "" {
		return errors.New("credential store requires a log destination for its audit records")
	}
	log, e := logger.New(cfg.Logger)
	if e != nil {
		return e
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	// The startup gate runs before the listener is bound. A missing or unusable key-encryption key, or
	// a volume other local accounts can read, refuses here (D6, D8) instead of coming up as an empty
	// store that reports "cannot decrypt" as "never existed".
	store, e := cfg.Store()
	if e != nil {
		return e
	}
	serverTLS, e := cfg.ServerTLS()
	if e != nil {
		return e
	}
	server, e := credentialstore.NewServer(store, credentialstore.ServerConfig{
		TLS:        serverTLS,
		Identities: cfg.Identities(),
		Log:        log,
	})
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", cfg.Listen)
	if e != nil {
		return e
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	failed := make(chan error, 1)
	go func() {
		// Only the key identifier is logged: it names a key without revealing one, and the volume and
		// listen address are locations rather than secrets.
		log.Info("credential store listening",
			zap.String("address", listener.Addr().String()),
			zap.String("volume", store.Volume()),
			zap.String("keyId", store.KeyID()),
		)
		// The mutual-TLS configuration wraps the listener directly rather than going through ServeTLS,
		// whose empty-file-name form silently depends on the certificate already being in the config.
		failed <- httpServer.Serve(tls.NewListener(listener, serverTLS))
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
		return httpServer.Shutdown(shutdown)
	}
}
