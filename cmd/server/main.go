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
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/collab"
	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/controlgrpc"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

// configureCollaboration installs the optional development Agent/Team/Workflow fixtures on the store
// when the deployment explicitly enables them (`collaboration.development_fixtures`). It is the
// cmd/server composition gate: production default is OFF, leaving the collaboration ports nil so the
// target discovery API serves only human targets. It is intentionally independent of authentication —
// enabling GitHub Auth must never enable these fixtures, and an auth failure must never fall back to
// a fixture identity.
func configureCollaboration(store *core.Store, developmentFixtures bool, log *zap.Logger) {
	if !developmentFixtures {
		// Production: Agent is the one collaboration target with real backing (space_agents). Wire a
		// roster-backed directory so target discovery serves real Space Agents; Team/Workflow
		// (cross-module ports, still unbuilt) stay unreported. Run-create resolution re-validates
		// against space_agents in-transaction regardless of this directory.
		if store.Pool != nil {
			store.Directory = core.NewSpaceAgentDirectory(store.Pool)
		}
		return
	}
	collab.WireDevelopmentFixtures(store)
	log.Warn("development collaboration fixtures enabled: Agent/Team targets served from in-memory fixtures (workflow targets stay database-backed; development-only — production must leave collaboration.development_fixtures false)")
}

func run() (runErr error) {
	configPath := flag.String("config", "", "configuration file")
	flag.Parse()
	cfg, e := config.Load(*configPath)
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
	store, e := core.NewStore(db)
	if e != nil {
		return e
	}
	defer func() { runErr = errors.Join(runErr, store.Pool.Close()) }()
	store.ObjectStore, e = cfg.ObjectStore.Open()
	if e != nil {
		return e
	}
	// The Credential Service ingress is optional: an unconfigured deployment gets no client and every
	// credential write refuses with `credential_store_unavailable`. An enabled but unusable section is
	// a startup error, not a per-request one, because D6 makes "a capability that cannot be
	// established" a refusal to start rather than a runtime downgrade.
	store.Secrets, e = cfg.Credentials.Open()
	if e != nil {
		return e
	}
	// The credentialed-executor boundary is optional in the same way and for the same reason. Its two
	// capabilities are switched on independently and each installs its own caller; the transport and the
	// signing key are shared, because one executor serves both routes and one key authorizes both.
	//
	// Unconfigured — the default — every Project that names a credential stays refused with
	// `repository_credential_executor_unavailable` on every path, and no mirror is prepared. Configured,
	// Cloud signs one single-use authorization per admitted operation and dispatches it to the executor,
	// and the token never passes through Cloud at all. An enabled but unusable section is a startup
	// error rather than a runtime downgrade.
	dispatch, e := cfg.ExecutorDispatch.Open()
	if e != nil {
		return e
	}
	if dispatch.Clone != nil {
		store.SetCredentialedCloner(&core.CloneDispatcher{
			Client:     dispatch.Client,
			SigningKey: dispatch.SigningKey,
			Lifetime:   dispatch.Clone.Lifetime,
		})
	}
	if dispatch.Mirror != nil {
		store.SetMirrorPreparer(&core.MirrorDispatcher{
			Client:           dispatch.Client,
			SigningKey:       dispatch.SigningKey,
			ExecutorIdentity: dispatch.Mirror.ExecutorIdentity,
			Lifetime:         dispatch.Mirror.Lifetime,
		})
	}
	// The mirror worker's loop is started below with the other dispatch loops, gated on the same
	// `dispatch.Mirror` the install above uses.
	//
	// Where a sandbox reads a published mirror is the deployment's other half, and the two are checked
	// together: a deployment that names the internal Git Server serves every credentialed Workspace's
	// clone from the mirror, so the credentialed-clone executor is not a second capability to install
	// beside it but the other channel, and naming the Git Server with no publisher is a channel that can
	// never open. Both are startup errors rather than per-request ones, for the same reason an enabled
	// but unusable section is. Unconfigured — the default — `endpoint` is nil and nothing about the
	// clone path changes.
	endpoint, e := cfg.MirrorEndpoint(dispatch)
	if e != nil {
		return e
	}
	store.SetMirrorEndpoint(endpoint)
	configureCollaboration(store, cfg.Collaboration.DevelopmentFixtures, log)
	// The B side of the Agent Run seam: Cloud's IssueRun/Thread lifecycle is driven by these five
	// callbacks, each of which runs inside the control-plane transaction that just persisted the
	// evidence it reacts to (controller-integration D6). They are the only writer of the B-side
	// business tables, and the control plane is the only writer of the execution evidence they read —
	// no second dispatcher, no second state machine. Leaving them unset keeps the A side a no-op
	// handoff, which is what a control-plane-only deployment wants.
	core.BindBusinessHooks(store)
	if e := store.CheckSchema(ctx); e != nil {
		return e
	}
	auth, e := core.NewAuthenticator(cfg.Auth.Audience, cfg.Auth.Keys)
	if e != nil {
		return e
	}
	var directory router.Directory
	if cfg.Directory.Endpoint != "" {
		key, readErr := os.ReadFile(cfg.Directory.AppKeyFile)
		if readErr != nil {
			return fmt.Errorf("read directory app key: %w", readErr)
		}
		directory, e = router.NewTianzhouClient(cfg.Directory.Endpoint, cfg.Directory.HWID, cfg.Directory.Environment, string(key), nil)
		if e != nil {
			return e
		}
	}
	// The marketplace sync loop is owned by this process like gateway.RunCleanup:
	// the ctx cancellation on shutdown stops it and the WaitGroup below waits
	// for the in-flight sync (network + scan only; no database transaction
	// spans them) before the process exits.
	var syncGroup sync.WaitGroup
	if cfg.Plugins.SyncEnabled {
		source := pluginmarket.Source{Namespace: "official", URL: cfg.Plugins.MarketplaceURL, Branch: cfg.Plugins.MarketplaceBranch}
		syncer := pluginmarket.NewSyncer(source, filepath.Join(os.TempDir(), "ora-cloud-plugins", "official"), store, log)
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, syncer.Sync, cfg.Plugins.SyncInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// B-owned dispatch loop for queued real Space Agent runs: a bounded rescan (limit
	// agentDispatchBatchSize, ordered by created_at,id) that moves a queued run to
	// phase='provisioning', status='dispatched' only when the A side accepts, and leaves busy runs
	// queued to retry at the next tick. It is owned by the process lifecycle like the marketplace
	// loop: ctx cancellation stops it and the WaitGroup below waits for the in-flight pass (each
	// dispatch runs in its own short transaction; no transaction spans the sleep). The 10s cadence
	// means a project that was busy at claim time unblocks within a tick.
	{
		const agentDispatchInterval = 10 * time.Second
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.DispatchQueuedAgentRunsOnce, agentDispatchInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// Session-start loop: B-owned recovery for runs settled into phase='starting' (their run
	// Workspace is provisioned and admitted). A bounded rescan releases each run's exactly-once first
	// produce: thread_entries seq=1 plus one EnqueueExecutionWork item in one transaction. The run
	// leaves 'starting' only on takeover evidence, so this loop stays idempotent alongside the
	// dispatch loop.
	{
		const agentSessionStartInterval = 10 * time.Second
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.StartQueuedAgentSessionsOnce, agentSessionStartInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// Mirror preparation: the loop that makes the preparer installed above reachable at all. Without it
	// the gate on the `node` -> `clone` transition refuses with `mirror_not_ready` forever, because no
	// preparation is ever claimed; an unconfigured deployment therefore has no loop, no claim, and no
	// half-prepared Operation.
	//
	// A pass dispatches serially — claim, dispatch, settle, next — which is deliberate: the claim is
	// what starts that attempt's lease, so claiming each row only immediately before dispatching it is
	// what keeps a lease from running while its attempt waits behind other work. The cost is that one
	// batch takes as long as its fetches do, so a backlog drains over passes rather than within one, and
	// the interval decides only how long an idle queue waits before it is looked at again.
	if dispatch.Mirror != nil {
		const mirrorPreparationInterval = 10 * time.Second
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.PrepareMirrorsOnce, mirrorPreparationInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// Thread-ending loops: B-owned recovery for the two ending triggers reachable without a new
	// public API. The idle scan ends a Thread whose idle window has expired (thread_state='idle' with
	// idle_since older than cfg.IssueRuns.ThreadIdleTimeout), and the cancel pass reacts to a
	// cancellation request recorded on a live run. Each run is handled in its own short transaction
	// and each transition is a CAS, so a repeated tick emits no second EndSession. Both stop at
	// thread_state='ending': the session terminal (`ended`) and everything after it belong to the
	// delivery loops below.
	{
		const agentThreadEndInterval = 10 * time.Second
		store.ThreadIdleTimeout = cfg.IssueRuns.ThreadIdleTimeout
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.EndIdleAgentThreadsOnce, agentThreadEndInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.ReactToCancelledAgentRunsOnce, agentThreadEndInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// Delivery/release loops. The first applies IssueRun D5's give-up limits to runs still
	// `delivering`: once delivery has failed for cfg.IssueRuns.DeliveryGiveUpAfter, or the run
	// Workspace's Node has been unreachable for cfg.IssueRuns.DeliveryUnreachableAfter, the run is
	// released with deliveryState=failed and its Workspace delete declared. The second reconciles the
	// release half: a `releasing` run whose delete_workspace operation has no operation in flight gets
	// its delete re-declared, which is the Cloud-side retry the decision requires after a Node refuses
	// to quiesce (the run Workspace has no public API and no user to retry it). The third applies
	// IssueRun D8's single give-up limit to the delete side (G-032): a `releasing` run whose Workspace
	// Node has been unknown for cfg.IssueRuns.DeliveryUnreachableAfter — the same window, D8 adds no
	// new one — is settled `done` with failure_reason=workspace_unavailable, its delete operation left
	// terminally failed and its Workspace row retained; `done` there means Cloud stopped retrying,
	// never that the Workspace was released. All three are bounded passes of one short transaction per
	// run, so a repeated tick produces no second release, no second delete operation, and no second
	// phase transition.
	{
		const agentDeliveryInterval = 10 * time.Second
		store.DeliveryGiveUpAfter = cfg.IssueRuns.DeliveryGiveUpAfter
		store.DeliveryUnreachableAfter = cfg.IssueRuns.DeliveryUnreachableAfter
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.GiveUpStaleDeliveriesOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.RedeclareRunWorkspaceDeletesOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.GiveUpStaleWorkspaceReleasesOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
	}
	gin.SetMode(cfg.Server.Mode)
	server := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Server.Port), Handler: router.New(store, auth, log, directory), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout, IdleTimeout: 60 * time.Second}
	// The control listener is bound before serving so a taken port fails startup, not a Controller.
	control, e := net.Listen("tcp", cfg.Control.GRPCAddr)
	if e != nil {
		return e
	}
	managementTLS, e := cfg.Control.TLS()
	if e != nil {
		return e
	}
	grpcServer, e := controlgrpc.NewSecure(store, managementTLS, cfg.Control.ControllerIdentity)
	if e != nil {
		return e
	}
	failed := make(chan error, 2)
	go func() {
		log.Info("Cloud listening", zap.String("address", server.Addr))
		failed <- server.ListenAndServe()
	}()
	go func() {
		log.Info("Cloud control listening", zap.String("address", control.Addr().String()))
		failed <- grpcServer.Serve(control)
	}()
	select {
	case e = <-failed:
		cancel()
		syncGroup.Wait()
		if errors.Is(e, http.ErrServerClosed) || errors.Is(e, grpc.ErrServerStopped) {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		// In-flight control calls finish or are cut at the same deadline as HTTP; a Controller
		// retries with the same submission identity, so cutting them loses nothing durable.
		// Tell the lease holder to stop claiming before its stream is cut; the Drain signal is a
		// hint, so a Controller that misses it simply fails its next claim against a stopped server.
		store.Signals.Drain()
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-shutdown.Done():
			grpcServer.Stop()
		}
		e = server.Shutdown(shutdown)
		syncGroup.Wait()
		return e
	}
}
