// Command clone-executor runs the credentialed-clone executor.
//
// It is the credential-executing identity C of the approved Node isolation decision
// (`20261010-controlled-node-git-authentication-isolation` D1, D7, D11), built as a process of the
// current product rather than as part of the historical Desktop Node. It runs as its own operating
// system identity, on its own volume, with its own trust root; it redeems one Cloud-signed
// single-use authorization as the `broker-resolver` identity, materializes the token where only it
// can read it, runs one constrained `git clone` through a deployment-fixed credential helper, and
// destroys the material before it answers.
//
// The command is deliberately thin. Every decision it could get wrong lives in internal/cloneexec and
// internal/config: the constrained Git invocation, the binding checks, the ordering of redemption and
// cleanup, and the startup gate that refuses to run without a verification key or a credential
// service. What is left here is the deployment's own configuration and the process lifecycle.
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
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/config"
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
	cfg, e := config.LoadCloneExecutorConfig(*configPath)
	if e != nil {
		return e
	}
	log, e := logger.New(cfg.Logger)
	if e != nil {
		return e
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()
	// The startup gate runs before the listener is bound. A missing verification key, an unusable
	// credential service endpoint, or a materialization root that is not an absolute path refuses here
	// rather than at the first job (D17).
	executor, e := cfg.Executor()
	if e != nil {
		return e
	}
	// The mirror capability is optional and absent by default. An empty mirror root yields no executor
	// and the route then answers `credential_executor_unavailable`; a declared but unbuildable mirror
	// section — a root nested inside the materialization root, a store whose limits are inconsistent —
	// refuses here, in the same startup gate as the clone path's.
	mirrorExecutor, e := cfg.MirrorExecutor()
	if e != nil {
		return e
	}
	// The push capability is optional in the same way and for a stronger reason: a controlled push is the
	// only thing this component does that changes something outside the platform, in a repository the
	// platform does not own. An empty push root yields no executor and the route then answers
	// `credential_executor_unavailable`; a declared but unbuildable push section — a scratch root that
	// contains or is contained by another capability's root — refuses here, in the same startup gate.
	pushExecutor, e := cfg.PushExecutor()
	if e != nil {
		return e
	}
	serverTLS, e := cfg.ServerTLS()
	if e != nil {
		return e
	}
	server, e := cloneexec.NewServer(executor, cloneexec.ServerConfig{
		TLS:    serverTLS,
		Caller: cfg.DispatcherIdentity,
		Mirror: mirrorExecutor,
		Push:   pushExecutor,
		Log:    log,
	})
	if e != nil {
		return e
	}
	// The sweep runs before the first job and before the listener is bound. It repairs the one failure
	// a running process cannot: a previous crash or a killed container left a token on disk with no
	// process left to destroy it, and only a start can (D13).
	removed, e := executor.Sweep()
	if e != nil {
		return e
	}
	fetched := 0
	if mirrorExecutor != nil {
		// The mirror sweep removes only the fetch directories of preparations that never finished. It
		// touches nothing published and does not reconcile the store, which is why it is safe at every
		// start while the store's own lifetime is left to the deployment.
		if fetched, e = mirrorExecutor.Sweep(); e != nil {
			return e
		}
	}
	pushed := 0
	if pushExecutor != nil {
		// A push's scratch repository has no life of its own, so anything under the push root at startup
		// is debris from a process that died — and debris that holds a plaintext copy of a private
		// upstream. Removing it is the whole repair, and it is why this runs before the listener is bound.
		if pushed, e = pushExecutor.Sweep(); e != nil {
			return e
		}
	}
	listener, e := net.Listen("tcp", cfg.Listen)
	if e != nil {
		return e
	}
	// The write timeout has to outlast the longest job the deployment can dispatch, because the response
	// is what the job produced. The two defaults are resolved separately before they are compared: a
	// deployment that configured a mirror root and left both timeouts at their defaults runs ten-minute
	// preparations, and comparing the raw zeros would have bounded them with the clone's five.
	jobTimeout := cloneexec.DefaultCloneTimeout
	if cfg.Timeout > 0 {
		jobTimeout = cfg.Timeout
	}
	if mirrorExecutor != nil {
		mirrorTimeout := cloneexec.DefaultMirrorTimeout
		if cfg.MirrorTimeout > 0 {
			mirrorTimeout = cfg.MirrorTimeout
		}
		jobTimeout = max(jobTimeout, mirrorTimeout)
	}
	if pushExecutor != nil {
		pushTimeout := cloneexec.DefaultPushTimeout
		if cfg.PushTimeout > 0 {
			pushTimeout = cfg.PushTimeout
		}
		jobTimeout = max(jobTimeout, pushTimeout)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      jobTimeout + time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	failed := make(chan error, 1)
	go func() {
		// Only locations are logged: the listen address, the roots, and how many leftovers the sweeps
		// removed. A credential and an authorization never reach a log line. The mirror root is logged as
		// an empty string when the capability is absent, which is a fact about the deployment rather than
		// a missing value.
		log.Info("clone executor listening",
			zap.String("address", listener.Addr().String()),
			zap.String("materialRoot", cfg.MaterialRoot),
			zap.String("checkoutRoot", cfg.CheckoutRoot),
			zap.String("mirrorRoot", cfg.MirrorRoot),
			zap.Bool("mirrorCapability", mirrorExecutor != nil),
			zap.String("pushRoot", cfg.PushRoot),
			zap.Bool("pushCapability", pushExecutor != nil),
			zap.Int("sweptEntries", removed),
			zap.Int("sweptFetches", fetched),
			zap.Int("sweptPushes", pushed),
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
