// Command git-server serves Project repositories to sandboxes over smart HTTP, answers Cloud's
// read-only delivery-ref query, and serves published mirrors to Workspaces. Git also runs it as the
// pre-receive hook (-pre-receive).
//
// It is one process with two surfaces and two trust models, and `internal/gitserver` records why they
// cannot share a listener: the repository surface authenticates a caller by an mTLS client
// certificate that names a runtime target and can write; the mirror surface authenticates a caller by
// the address its connection arrived from and can only read. What is left here is configuration,
// dependency wiring, lifecycle and exit behavior — which surfaces this deployment turned on, where
// each listens, and which identity each presents.
//
// The repository surface is on unless the deployment says otherwise (GIT_SERVER_REPOSITORY_ENABLED),
// because the fail-closed direction for a deployment that did not mention it is to demand the
// material it needs or refuse to start. The mirror surface is off unless the deployment names a
// mirror volume (-mirror-root), because having mirrors to serve is what opting into it means.
//
// It holds no upstream credential. The commit it serves was fetched by the credentialed executor,
// which is the only component that ever held one (D1).
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
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run owns every listener this deployment asked for and the hook channel; shutdown drains HTTP before
// closing the channel that in-flight pushes still need for their pre-receive decision.
func run() (runErr error) {
	path := flag.String("config", "", "Cloud configuration file; supplies the database both surfaces authorize from")
	health := flag.Bool("healthcheck", false, "verify readiness through the ref listener's health route")
	preReceive := flag.Bool("pre-receive", false, "run as the pre-receive hook (invoked by git)")
	mirrorSurface := mirrorFlags()
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
		// Readiness is served by the ref listener, and only the repository surface opens one. A
		// deployment that turned that surface off has no health route, and reporting "not ready"
		// forever would be reporting a component it never asked to run.
		if !deployment.RepositoryEnabled {
			return errors.New("-healthcheck verifies the ref listener, and this process serves no repository surface")
		}
		return checkHealth(&deployment)
	}
	if err := mirrorSurface.validate(); err != nil {
		return err
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
	// The reclaim loop writes through this adapter rather than through a logger of its own, so both
	// surfaces and the housekeeping between them land in one stream an operator reads in one place.
	values := logger.NewValues(log)

	options := &gitserver.Options{GitProgram: deployment.GitProgram, Health: store.Pool.PingContext, Logger: log}
	var repositoryTLS, refTLS *tls.Config
	if deployment.RepositoryEnabled {
		// Read before the service is built so that a deployment which cannot present its identity, or
		// name the one certificate allowed to ask about refs, hears about it instead of serving.
		cloudClient, err := deployment.CloudClient()
		if err != nil {
			return err
		}
		if repositoryTLS, err = deployment.RepositoryTLS(); err != nil {
			return err
		}
		if refTLS, err = deployment.RefTLS(); err != nil {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		options.Repository = &gitserver.RepositoryOptions{
			Store:          store,
			RepositoryRoot: deployment.RepositoryRoot,
			CloudClient:    cloudClient,
			// The hook is this binary, and the path is this process's own, so a deployment cannot
			// point a repository at a hook it did not install.
			Hook: []string{self, "-pre-receive"},
		}
	}
	if mirrorSurface.enabled() {
		// The mirror store refuses to open a volume it cannot prove it owns, and validates its own
		// capacity limits. A Git Server with no usable mirror volume has nothing to serve and must not
		// come up as one that answers "forbidden" to everything, which is indistinguishable from a
		// working server refusing correctly.
		mirrors, err := mirror.New(mirror.Config{
			Root:           mirrorSurface.root,
			MaxMirrorBytes: mirrorSurface.maxMirrorBytes,
			MaxVolumeBytes: mirrorSurface.maxVolumeBytes,
			Grace:          mirrorSurface.grace,
			Clock:          time.Now,
		})
		if err != nil {
			return err
		}
		// The Git Server's own identity toward the resolver, which is not the Controller's: this
		// process resolves one address per read request and nothing else.
		origins, err := gitserver.NewOriginClient(mirrorSurface.originURL, gitserver.OriginIdentity{
			CertFile:          filepath.Join(mirrorSurface.identityDir, "client.pem"),
			KeyFile:           filepath.Join(mirrorSurface.identityDir, "client.key"),
			CAFile:            filepath.Join(mirrorSurface.identityDir, "ca.pem"),
			ServerCertificate: filepath.Join(mirrorSurface.identityDir, "origin.pem"),
			ServerName:        mirrorSurface.originName,
		}, mirrorSurface.originTimeout)
		if err != nil {
			return err
		}
		state, err := gitserver.MirrorState(store, mirrors)
		if err != nil {
			return err
		}
		options.Mirror = &gitserver.MirrorOptions{Mirrors: mirrors, Origins: origins, State: state, Timeout: mirrorSurface.timeout}
		if mirrorSurface.reclaim {
			// The loop is a goroutine of this process, so it is started here and stopped by the
			// shutdown below. It is off unless the deployment asked for it: with the flag unset this
			// process removes nothing it did not remove before the loop existed.
			reclaimCtx, stopReclaim := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				runMirrorReclaim(reclaimCtx, store.Pool, mirrors, mirrorSurface.reclaimEvery, values)
			}()
			// Stopped and awaited before the deferred pool close runs, because a pass in flight holds a
			// connection and closing the pool under it would report a failure on the way out of a
			// shutdown that was not one. The wait is bounded: canceling the context ends every database
			// call the pass makes, so what could still outlast it is a volume walk, and a shutdown must
			// not be held open by housekeeping.
			defer func() {
				stopReclaim()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					log.Warn("the mirror reclaim pass did not stop in time; exiting anyway")
				}
			}()
			log.Info("mirror reclaim enabled", zap.Duration("interval", mirrorSurface.reclaimEvery), zap.Duration("grace", mirrorSurface.grace))
		}
	}
	service, err := gitserver.New(options)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, service.Close()) }()

	// Which surfaces exist was decided above, and this is the whole of what the process listens on:
	// one listener per surface, each presenting the identity that surface's callers expect, and no
	// listener at all for a surface this deployment did not turn on. The repository surface's two
	// listeners are upstream's, unchanged in address and TLS posture.
	servers := make([]*http.Server, 0, 3)
	if deployment.RepositoryEnabled {
		servers = append(servers,
			&http.Server{Addr: deployment.RepositoryAddress, Handler: http.HandlerFunc(service.RepositoryHandler), TLSConfig: repositoryTLS},
			&http.Server{Addr: deployment.RefAddress, Handler: http.HandlerFunc(service.RefHandler), TLSConfig: refTLS},
		)
	}
	if mirrorSurface.enabled() {
		mirrorTLS, err := mirrorListenerTLS(mirrorSurface.certFile, mirrorSurface.keyFile)
		if err != nil {
			return err
		}
		servers = append(servers, &http.Server{Addr: mirrorSurface.address, Handler: http.HandlerFunc(service.MirrorHandler), TLSConfig: mirrorTLS})
	}
	for _, server := range servers {
		// No write timeout: pack transfers are bounded by the client and by request cancellation.
		server.ReadHeaderTimeout = 5 * time.Second
		server.IdleTimeout = time.Minute
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		log.Info("git server listening", zap.String("address", server.Addr))
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

// mirrorSettings is everything the mirror surface was told. It is one value rather than a flag
// variable per case so that "did this deployment ask for the surface, and is it completely
// configured?" is answered once, by validate, instead of by the wiring reading flags in whatever
// order it happens to reach them.
type mirrorSettings struct {
	address        string
	root           string
	originURL      string
	identityDir    string
	originName     string
	certFile       string
	keyFile        string
	timeout        time.Duration
	originTimeout  time.Duration
	maxMirrorBytes int64
	maxVolumeBytes int64
	grace          time.Duration
	reclaim        bool
	reclaimEvery   time.Duration
}

// mirrorFlags registers the mirror surface's flags and returns them. The value is a pointer because
// the flag package must keep writing into the same struct the caller reads.
func mirrorFlags() *mirrorSettings {
	s := &mirrorSettings{}
	flag.StringVar(&s.address, "mirror-addr", "127.0.0.1:18002", "listen address of the Workspace-facing mirror listener")
	flag.StringVar(&s.root, "mirror-root", "", "directory holding the mirror store; naming it is what turns the mirror surface on")
	flag.StringVar(&s.originURL, "origin-url", "", "origin resolver base URL, e.g. https://sandbox-server:18001")
	flag.StringVar(&s.identityDir, "identity-dir", "", "directory holding this surface's identity toward the resolver: client.pem, client.key, ca.pem, origin.pem")
	flag.StringVar(&s.originName, "origin-server-name", "", "name the origin resolver's certificate must carry")
	flag.StringVar(&s.certFile, "mirror-tls-cert", "", "certificate the mirror listener presents")
	flag.StringVar(&s.keyFile, "mirror-tls-key", "", "private key for -mirror-tls-cert")
	flag.DurationVar(&s.timeout, "mirror-timeout", 5*time.Minute, "time allowed for one clone on the mirror listener")
	flag.DurationVar(&s.originTimeout, "origin-timeout", 10*time.Second, "time allowed for one origin resolution")
	flag.Int64Var(&s.maxMirrorBytes, "max-mirror-bytes", 4<<30, "largest single mirror; reaching it fails the mirror instead of publishing a partial one")
	flag.Int64Var(&s.maxVolumeBytes, "max-volume-bytes", 64<<30, "largest total the mirror volume may hold")
	flag.DurationVar(&s.grace, "mirror-grace", 24*time.Hour, "how long an unreferenced mirror is kept before it may be reclaimed")
	flag.BoolVar(&s.reclaim, "mirror-reclaim", false, "reclaim mirrors Cloud no longer references; off unless the deployment asks for it")
	flag.DurationVar(&s.reclaimEvery, "mirror-reclaim-interval", 5*time.Minute, "how often the reclaim pass runs when -mirror-reclaim is set")
	return s
}

// enabled reports whether the deployment asked for the mirror surface at all. Naming the volume is
// what turns it on: a mirror surface without a volume has nothing to serve, so there is no second
// flag that could disagree with this one.
func (s *mirrorSettings) enabled() bool { return s.root != "" }

// validate refuses an incomplete or contradictory mirror surface at startup, and it says nothing
// about a surface this deployment did not ask for — a value nothing consults is not a value to fail
// a startup over.
func (s *mirrorSettings) validate() error {
	if !s.enabled() {
		if s.reclaim {
			return errors.New("-mirror-reclaim needs -mirror-root: the reclaim loop sweeps the mirror volume and there is none")
		}
		return nil
	}
	if s.address == "" {
		return errors.New("-mirror-addr is required by the mirror surface")
	}
	if s.originURL == "" || s.identityDir == "" || s.originName == "" {
		return errors.New("-origin-url, -identity-dir and -origin-server-name are required by the mirror surface: a Git Server that cannot identify its callers must not start")
	}
	// Both halves of the pair, always. This surface's only client is a Node, whose clone policy
	// permits it to verify an https endpoint and nothing else, so a plaintext listener would be one
	// no caller could reach — and a listener nobody can reach is not a safe default, it is a
	// misconfiguration that looks like a working one.
	if s.certFile == "" || s.keyFile == "" {
		return errors.New("-mirror-tls-cert and -mirror-tls-key are required by the mirror surface")
	}
	// A zero or negative interval would turn the reclaim pass into a busy loop against PostgreSQL and
	// the mirror volume. It is refused rather than clamped, because a deployment that asked for a
	// period it did not get should hear about it instead of running at one it never chose.
	if s.reclaim && s.reclaimEvery <= 0 {
		return errors.New("-mirror-reclaim-interval must be positive")
	}
	return nil
}

// mirrorListenerTLS builds the mirror listener's TLS configuration from its own certificate pair.
//
// Only the server's identity is configured, and that is the design rather than an omission: the Node
// that clones holds no certificate — Cloud gives it a clone URL and a CA to verify, and nothing else
// — so who the caller is comes from the address the connection arrived from, resolved per request
// (D3). Client authentication is not relaxed here; there is nothing to relax, and the repository
// listener's requirement is untouched by this one existing.
func mirrorListenerTLS(certFile, keyFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the mirror listener certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
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
