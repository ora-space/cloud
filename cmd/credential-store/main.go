// Command credential-store runs the independent Credential Store: four TLS 1.3 listeners
// (redemption, management, key, browser ingress) over one private volume, plus a periodic sweep
// that destroys superseded versions past their retention.
//
// The command only loads configuration, wires dependencies and owns the process lifecycle; every
// policy lives in internal/credentialstore. It refuses to start without a usable keyring, Cloud's
// two verification key sets, or a configuration whose listeners would trust overlapping identities.
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

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/logger"
)

// sweepInterval is how often superseded versions and expired claims are swept. Redemption refuses
// an expired version on its own, so the interval only bounds how long its ciphertext lingers.
const sweepInterval = 5 * time.Minute

// shutdownTimeout bounds draining the listeners on SIGTERM.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	cfg, err := credentialstore.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	log, err := logger.New(logger.Config{Level: cfg.LogLevel, EnableConsole: true})
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, logger.Sync(log)) }()

	// Startup gate: no keyring, no keys, no service: an empty store would report "cannot decrypt" as
	// "never existed".
	keyring, err := credentialstore.LoadKeyring(cfg.KeyringFile)
	if err != nil {
		return err
	}
	store, err := credentialstore.OpenStore(credentialstore.StoreConfig{Root: cfg.Root, Keyring: keyring, Now: time.Now})
	if err != nil {
		return err
	}
	executionKeys, err := cloneauth.LoadPublicKeys(cfg.ExecutionPublicKeysFile)
	if err != nil {
		return fmt.Errorf("execution public keys: %w", err)
	}
	submissionKeys, err := cloneauth.LoadPublicKeys(cfg.SubmissionPublicKeysFile)
	if err != nil {
		return fmt.Errorf("submission public keys: %w", err)
	}
	server, err := credentialstore.NewServer(&credentialstore.ServerOptions{
		Store: store, ExecutionKeys: executionKeys, SubmissionKeys: submissionKeys,
		Identities: cfg.Identities, AllowedOrigins: cfg.IngressAllowedOrigins,
		RatePerMinute: cfg.IngressRatePerMinute, RateBurst: cfg.IngressRateBurst, Logger: log,
	})
	if err != nil {
		return err
	}
	serviceTLS, err := cfg.ServiceTLS()
	if err != nil {
		return err
	}
	ingressTLS, err := cfg.IngressTLS()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	servers := []*http.Server{
		{Addr: cfg.RedemptionAddr, Handler: server.RedemptionHandler(), TLSConfig: serviceTLS},
		{Addr: cfg.ManagementAddr, Handler: server.ManagementHandler(), TLSConfig: serviceTLS.Clone()},
		{Addr: cfg.KeyAddr, Handler: server.KeyHandler(), TLSConfig: serviceTLS.Clone()},
		{Addr: cfg.IngressAddr, Handler: server.IngressHandler(), TLSConfig: ingressTLS},
	}
	// Bind every listener before serving any, so a port conflict fails startup as a whole.
	listeners := make([]net.Listener, 0, len(servers))
	defer func() {
		for _, l := range listeners {
			_ = l.Close() // already closed by Shutdown on the normal path
		}
	}()
	for _, s := range servers {
		s.ReadHeaderTimeout = 5 * time.Second
		s.ReadTimeout = 30 * time.Second
		s.WriteTimeout = 30 * time.Second
		s.IdleTimeout = time.Minute
		s.MaxHeaderBytes = 32 << 10
		s.BaseContext = func(net.Listener) context.Context { return ctx }
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", s.Addr, err)
		}
		listeners = append(listeners, l)
	}
	log.Info("credential store started",
		zap.String("redemptionAddr", cfg.RedemptionAddr), zap.String("managementAddr", cfg.ManagementAddr),
		zap.String("keyAddr", cfg.KeyAddr), zap.String("ingressAddr", cfg.IngressAddr),
		zap.String("kekId", store.KeyID()))

	failed := make(chan error, len(servers))
	for i, s := range servers {
		go func() { failed <- s.Serve(tls.NewListener(listeners[i], s.TLSConfig)) }()
	}
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweep(ctx, store, log)
	}()

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
	shutdown, stop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stop()
	for _, s := range servers {
		err = errors.Join(err, s.Shutdown(shutdown))
	}
	for range waits {
		select {
		case <-failed:
		case <-shutdown.Done():
			return errors.Join(err, shutdown.Err())
		}
	}
	<-sweepDone
	return err
}

// sweep runs the retention sweep at startup and then every sweepInterval until ctx ends. A failed
// sweep is logged and retried at the next tick: it delays destruction, it never makes an expired
// version redeemable.
func sweep(ctx context.Context, store *credentialstore.Store, log *zap.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		result, err := store.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("credential store sweep failed", zap.Error(err))
		} else if result.Versions > 0 || result.Claims > 0 {
			log.Info("credential store sweep", zap.Int("versions", result.Versions), zap.Int("claims", result.Claims))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
