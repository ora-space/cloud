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
	"github.com/wanglongan587/cloud/internal/modelgateway"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
	"github.com/wanglongan587/cloud/internal/repository"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

// configureCollaboration installs the optional development Agent/Team/Workflow fixtures on the store
// when the deployment explicitly enables them (`collaboration.development_fixtures`). It is the
// cmd/server composition gate: production default is OFF, where the directory serves the two real
// target types (Space Agents and workflows) and the fixture-only ports stay nil. It is intentionally
// independent of authentication — enabling GitHub Auth must never enable these fixtures, and an auth
// failure must never fall back to a fixture identity.
func configureCollaboration(store *core.Store, developmentFixtures bool, log *zap.Logger) {
	if !developmentFixtures {
		// Production: layer the roster-backed Agent directory in front of the workflow-backed one
		// NewStore already installed, so discovery and resolution serve both real target types.
		// Replacing the directory instead would drop published workflows from @ — the regression #65
		// reported. Team has no backend in any deployment and stays unreported. Run-create resolution
		// re-validates agents against space_agents in-transaction regardless of this directory.
		if store.Pool != nil {
			store.Directory = core.DirectoryChain{core.NewSpaceAgentDirectory(store.Pool), store.Directory}
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
	store.GitServer, e = cfg.GitServer.Open()
	if e != nil {
		return e
	}
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
	// Workflow asset import is triggered by the space's desired state, never by a Node install
	// result: a space with no live runtime still receives its templates. Downloads run outside
	// transactions; only results are written. The checked outbound client enforces the same
	// public-address policy as the model gateway and never follows redirects.
	if cfg.Plugins.AssetImportEnabled {
		upstream, clientErr := modelgateway.NewUpstreamClient(modelgateway.UpstreamConfig{DNS: modelgateway.DefaultDNSConfig()})
		if clientErr != nil {
			return clientErr
		}
		store.WorkflowAssets = workflowasset.NewFetcher(store.ObjectStore, upstream.Transport)
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.ImportWorkflowAssetsOnce, cfg.Plugins.AssetImportInterval, pluginmarket.ContextSleep, log)
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
		store.AgentStartTimeout = cfg.IssueRuns.AgentStartTimeout
		store.AgentRestoreTimeout = cfg.IssueRuns.AgentRestoreTimeout
		store.AgentEndConfirmTimeout = cfg.IssueRuns.AgentEndConfirmTimeout
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.StartQueuedAgentSessionsOnce, agentSessionStartInterval, pluginmarket.ContextSleep, log)
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
	// IssueRun D8's single give-up limit to the delete side: a `releasing` run whose Workspace
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
