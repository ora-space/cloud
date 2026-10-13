// Command git-server runs the read-only internal Git Server.
//
// It is a separate process in this repository (git-server D1) and it is off by default: it is in no
// deployment manifest, publishes no port, and refuses every request until it is wired with an origin
// resolver identity, Cloud's database and a populated mirror store. Nothing here enables it, and
// nothing here relaxes the fail-closed behavior it starts with.
//
// The command is deliberately thin. Every decision it could get wrong lives in internal/gitserver,
// internal/gitread, internal/mirror and internal/core: which services are served, how a caller is
// identified, what Cloud's authoritative state is for one request, and when a mirror is readable.
// What is left here is configuration, dependency wiring, lifecycle and exit behavior.
//
// It holds no upstream credential. The commit it serves was fetched by the credentialed executor,
// which is the only component that ever held one (D1).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-server:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "", "Cloud configuration file; supplies the database the read authorization is decided from")
		addr       = flag.String("addr", "127.0.0.1:18002", "listen address of the sandbox-facing read listener")
		mirrorRoot = flag.String("mirror-root", "", "directory holding the mirror store; required")
		originURL  = flag.String("origin-url", "", "origin resolver base URL, e.g. https://sandbox-server:18001; required")
		identityTo = flag.String("identity-dir", "", "directory holding the Git Server's own identity: client.pem, client.key, ca.pem, origin.pem; required")
		serverName = flag.String("origin-server-name", "", "name the origin resolver's certificate must carry; required")
		tlsCert    = flag.String("tls-cert", "", "certificate the sandbox-facing listener presents; with -tls-key it serves HTTPS")
		tlsKey     = flag.String("tls-key", "", "private key for -tls-cert")
		gitPath    = flag.String("git", "git", "git binary used for upload-pack")
		timeout    = flag.Duration("timeout", 5*time.Minute, "time allowed for one clone")
		originWait = flag.Duration("origin-timeout", 10*time.Second, "time allowed for one origin resolution")
		maxMirror  = flag.Int64("max-mirror-bytes", 4<<30, "largest single mirror; reaching it fails the mirror instead of publishing a partial one")
		maxVolume  = flag.Int64("max-volume-bytes", 64<<30, "largest total the mirror volume may hold")
		grace      = flag.Duration("mirror-grace", 24*time.Hour, "how long an unreferenced mirror is kept before it may be reclaimed")
		reclaim    = flag.Bool("mirror-reclaim", false, "reclaim mirrors Cloud no longer references; off unless the deployment asks for it")
		reclaimFor = flag.Duration("mirror-reclaim-interval", 5*time.Minute, "how often the reclaim pass runs when -mirror-reclaim is set")
	)
	flag.Parse()
	if *mirrorRoot == "" {
		return errors.New("-mirror-root is required")
	}
	// A zero or negative interval would turn the reclaim pass into a busy loop against PostgreSQL and
	// the mirror volume. It is refused at startup rather than clamped, because a deployment that asked
	// for a period it did not get should hear about it instead of running at one it never chose.
	if *reclaim && *reclaimFor <= 0 {
		return errors.New("-mirror-reclaim-interval must be positive")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return serve(settings{
		configPath: *configPath, addr: *addr, mirrorRoot: *mirrorRoot, originURL: *originURL,
		identityDir: *identityTo, serverName: *serverName, tlsCert: *tlsCert, tlsKey: *tlsKey,
		gitPath: *gitPath, timeout: *timeout,
		originTimeout: *originWait, maxMirrorBytes: *maxMirror, maxVolumeBytes: *maxVolume, grace: *grace,
		mirrorReclaim: *reclaim, reclaimInterval: *reclaimFor,
	}, log)
}

// listenerTLS builds the sandbox-facing listener's TLS configuration from the Git Server's own
// certificate pair.
//
// Only the server's identity is configured, and that is the design rather than an omission: the
// sandbox that clones holds no certificate — Cloud gives it a clone URL and a CA to verify, and
// nothing else — so who the caller is comes from the address the connection arrived from, resolved
// per request (D3). A listener without TLS is not an option either: Git's own policy prefix in the
// sandbox allows only https, so a plaintext endpoint is unreachable by the client it exists for.
func listenerTLS(certFile, keyFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the listener certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
}

// settings is everything the process was told, gathered so the wiring below takes one argument
// rather than eight.
type settings struct {
	configPath      string
	addr            string
	mirrorRoot      string
	originURL       string
	identityDir     string
	serverName      string
	tlsCert         string
	tlsKey          string
	gitPath         string
	timeout         time.Duration
	originTimeout   time.Duration
	maxMirrorBytes  int64
	maxVolumeBytes  int64
	grace           time.Duration
	mirrorReclaim   bool
	reclaimInterval time.Duration
}

// the wiring below must be readable as one snapshot rather than as a live shared struct.
//
//nolint:gocritic // By value on purpose: what the process was told is fixed before it starts, and
func serve(s settings, log *slog.Logger) error {
	if s.originURL == "" || s.identityDir == "" || s.serverName == "" {
		return errors.New("-origin-url, -identity-dir and -origin-server-name are required: a Git Server that cannot identify its callers must not start")
	}
	// Half a key pair is refused rather than ignored: a deployment that meant to serve HTTPS and
	// mistyped one flag would otherwise come up on plaintext without saying so.
	if (s.tlsCert == "") != (s.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key are required together")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(s.configPath)
	if err != nil {
		return err
	}
	db, err := repository.InitDB(ctx, cfg.Database)
	if err != nil {
		return err
	}
	pool, err := db.DB()
	if err != nil {
		return fmt.Errorf("get the PostgreSQL pool: %w", err)
	}
	defer func() { _ = pool.Close() }()
	store, err := core.NewStore(db)
	if err != nil {
		return err
	}
	// The mirror store refuses to open a volume it cannot prove it owns, and validates its own
	// capacity limits. A Git Server with no usable mirror volume has nothing to serve and must not
	// come up as one that answers "forbidden" to everything, which is indistinguishable from a
	// working server refusing correctly.
	mirrors, err := mirror.New(mirror.Config{
		Root:           s.mirrorRoot,
		MaxMirrorBytes: s.maxMirrorBytes,
		MaxVolumeBytes: s.maxVolumeBytes,
		Grace:          s.grace,
		Clock:          time.Now,
	})
	if err != nil {
		return err
	}
	dir := s.identityDir
	origins, err := gitserver.NewOriginClient(s.originURL, gitserver.OriginIdentity{
		CertFile:          dir + "/client.pem",
		KeyFile:           dir + "/client.key",
		CAFile:            dir + "/ca.pem",
		ServerCertificate: dir + "/origin.pem",
		ServerName:        s.serverName,
	}, s.originTimeout, log)
	if err != nil {
		return err
	}
	state, err := gitserver.MirrorState(store, mirrors)
	if err != nil {
		return err
	}
	// The reclaim loop is a goroutine of this process, so it is started here and ends with the context
	// the shutdown below already cancels. It is off unless the deployment set -mirror-reclaim: with the
	// flag unset this process behaves exactly as it did before the loop existed, serving reads and
	// removing nothing.
	if s.mirrorReclaim {
		reclaim, cancelReclaim := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			runMirrorReclaim(reclaim, pool, mirrors, s.reclaimInterval, log)
		}()
		// The loop is stopped and awaited before the deferred pool close below runs, because a pass in
		// flight holds a connection and closing the pool under it would report a failure on the way out
		// of a shutdown that was not one. The wait is bounded: canceling the context ends every
		// database call the pass makes, so what could still outlast it is a volume walk, and a
		// shutdown must not be held open by housekeeping.
		defer func() {
			cancelReclaim()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				log.Warn("the mirror reclaim pass did not stop in time; exiting anyway")
			}
		}()
		log.Info("mirror reclaim enabled", "interval", s.reclaimInterval.String(), "grace", s.grace.String())
	}
	server, err := gitserver.New(gitserver.Config{
		Mirrors: mirrors, Origins: origins, State: state,
		GitPath: s.gitPath, Timeout: s.timeout, Logger: log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	if s.tlsCert != "" {
		serverTLS, err := listenerTLS(s.tlsCert, s.tlsKey)
		if err != nil {
			return err
		}
		// The TLS configuration wraps the listener directly rather than going through ServeTLS,
		// matching cmd/credential-store and cmd/clone-executor.
		listener = tls.NewListener(listener, serverTLS)
	}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() {
		log.Info("git server listening", "address", listener.Addr().String(), "mirrorRoot", s.mirrorRoot)
		failed <- httpServer.Serve(listener)
	}()
	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		log.Info("shutting down")
		return httpServer.Shutdown(shutdown)
	}
}
