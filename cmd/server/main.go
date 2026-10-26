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
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/egress"
	"github.com/wanglongan587/cloud/internal/logger"
	"github.com/wanglongan587/cloud/internal/modelgateway"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
	"github.com/wanglongan587/cloud/internal/repository"
	"github.com/wanglongan587/cloud/internal/repoworker"
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
	egressPath := flag.String("deployment-egress", os.Getenv("ORA_DEPLOYMENT_EGRESS_FILE"), "shared deployment egress JSON file")
	flag.Parse()
	policy, e := egress.Load(*egressPath)
	if e != nil {
		return e
	}
	if policy.Enterprise() && os.Getenv("MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS") != "" {
		return errors.New("enterprise egress conflicts with development hosts")
	}
	dns := egress.DeploymentDNSConfig()
	upstream, e := modelgateway.NewUpstreamClient(modelgateway.UpstreamConfig{DNS: dns, Policy: policy})
	if e != nil {
		return e
	}
	defer upstream.CloseIdleConnections()
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
	store.ModelEgress = policy
	store.ObjectStore, e = cfg.ObjectStore.Open()
	if e != nil {
		return e
	}
	store.GitServer, e = cfg.GitServer.Open()
	if e != nil {
		return e
	}
	configureCollaboration(store, cfg.Collaboration.DevelopmentFixtures, log)
	if cfg.Plugins.PersonalAgentTarget != "" {
		store.PersonalAgentRuntime = &core.PersonalAgentRuntime{Target: cfg.Plugins.PersonalAgentTarget, ContractVersion: 1, Protocols: []string{"openai-completions", "anthropic-messages"}}
	}
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
	// deployment policy as the model gateway and never follows redirects.
	if cfg.Plugins.AssetImportEnabled {
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
	// Blocked-creation loop (ADR D5): a run Workspace creation parked in `blocked` has no public
	// retry (the Workspace is invisible), so Cloud's own scan is the only way its wait ends. The
	// first pass anchors an absolute, never-renewed deadline (30m default); a later pass abandons
	// the business run, fails the creation operation, releases the Project operation slot and
	// retains an independent residual ledger — all in one transaction per run, so a repeated tick
	// confirms the same settlement instead of rebuilding anything. The residual Workspace stays
	// protected: no delete is queued, admission stays closed, and late Controller facts are
	// receipted through the provisioning-fact entry without resurrecting the run.
	{
		const agentWorkspaceBlockedInterval = 10 * time.Second
		store.AgentWorkspaceBlockedTimeout = cfg.IssueRuns.AgentWorkspaceBlockedTimeout
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.GiveUpBlockedWorkspaceCreationsOnce, agentWorkspaceBlockedInterval, pluginmarket.ContextSleep, log)
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
		const scheduledTaskLoopInterval = 10 * time.Second
		const scheduledTaskDeadlineLoopInterval = time.Minute
		store.ThreadIdleTimeout = cfg.IssueRuns.ThreadIdleTimeout
		store.ScheduledTaskAcceptanceEnabled = cfg.IssueRuns.ScheduledTaskAcceptanceEnabled
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
		// Cloud Scheduled Task (specs decisions/cloud/scheduled-task): the evaluation pass
		// admits due occurrences (D2/D3/D5) and the deadline pass writes the 60-minute
		// cancellation request and its scheduled-layer fact (D4) for the shared G-026 writer
		// machinery (the React loop above) to consume.
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.EvaluateScheduledTasksOnce, scheduledTaskLoopInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.WriteScheduledTaskDeadlinesOnce, scheduledTaskDeadlineLoopInterval, pluginmarket.ContextSleep, log)
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
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			// A scheduled run's artifact window is fixed when its declaration is stored, so expiry is
			// the same clock-driven pass as the others: one short transaction per run, no policy.
			pluginmarket.RunSyncLoop(ctx, store.ExpireScheduledArtifactDeliveriesOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			// Retention applies D5's ten-run cost boundary per task; the object-store deletions happen
			// between transactions, exactly like the reads.
			pluginmarket.RunSyncLoop(ctx, store.PruneScheduledRunArtifactsOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			// The prefix sweep is the lifetime fallback for objects a presigned PUT wrote after Cloud
			// stopped granting them: deterministic keys, no bucket listing, no receipt needed.
			pluginmarket.RunSyncLoop(ctx, store.CollectScheduledArtifactObjectsOnce, agentDeliveryInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// Residual recovery remains independent of a run's business terminal state. Cloud consumes
	// existing, identity-bound facts; a missing fact leaves its ledger open rather than treating
	// a timer or reconnection as cleanup proof. This loop shares cancellation and shutdown joining
	// with the delivery/release passes above and has no disable switch.
	syncGroup.Add(1)
	go func() {
		defer syncGroup.Done()
		pluginmarket.RunSyncLoop(ctx, store.ReconcileAgentRunResidualsOnce, cfg.IssueRuns.ResidualReconcileInterval, pluginmarket.ContextSleep, log)
	}()
	// Workflow advance loop: the Cloud-side scheduler (specs#99 D1 条件②/④, landing order step 3).
	// A bounded rescan of pending and running workflow runs, each advanced in its own short
	// transaction; the ready set is recomputed from committed state only, so the pass is idempotent
	// and a replica that crashes mid-tick resumes exactly where the committed state says. start,
	// condition and output execute inline; an agent node runs as a real session through the control
	// plane and settles from that session's terminal evidence. The settlement reads the Node
	// unreachability window the delivery loops above configure — the same `DeliveryUnreachableAfter`
	// D5 and D8 judge, not a second limit — so an in-flight session whose Node is gone fails its node
	// instead of holding the run in `running`, and an unconfigured window fails closed by holding it.
	// A session whose Node is still there but which produces neither a result nor a successful turn
	// end is bounded separately by cfg.Workflow.SessionTurnTimeout (circus#299 option A).
	{
		const workflowAdvanceInterval = 10 * time.Second
		store.WorkflowSessionTurnTimeout = cfg.Workflow.SessionTurnTimeout
		syncGroup.Add(1)
		go func() {
			defer syncGroup.Done()
			pluginmarket.RunSyncLoop(ctx, store.AdvanceWorkflowRunsOnce, workflowAdvanceInterval, pluginmarket.ContextSleep, log)
		}()
	}
	// The external Git line: the repository worker drives syncs, publishes and credential checks
	// through the Repo Executor, and the Store gets the Credential Store and ref-query ports its
	// credential and publish routes need. Owned like the loops above: ctx cancellation stops it
	// and the WaitGroup waits for in-flight tasks to settle.
	if err := wireRepository(ctx, &cfg.Repository, store, log, &syncGroup); err != nil {
		return err
	}
	gin.SetMode(cfg.Server.Mode)
	server := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Server.Port), Handler: egress.HealthMetadata(router.New(store, auth, log, directory), policy.Digest(dns)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout, IdleTimeout: 60 * time.Second}
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

// credentialStoreManager adapts the Credential Store's management client to the port core
// consumes, so core depends on no transport.
type credentialStoreManager struct {
	client *credentialstore.ManagementClient
}

func (m credentialStoreManager) CredentialVersion(ctx context.Context, id string) (core.StoredCredential, error) {
	md, err := m.client.Get(ctx, id)
	return core.StoredCredential{CurrentVersion: md.CurrentVersion, SubmissionID: md.SubmissionID, Revoked: md.Revoked}, err
}

func (m credentialStoreManager) RevokeCredential(ctx context.Context, id string) error {
	return m.client.Revoke(ctx, id)
}

func wireRepository(ctx context.Context, cfg *config.RepositoryConfig, store *core.Store, log *zap.Logger, group *sync.WaitGroup) error {
	opened, err := cfg.Open()
	if err != nil || opened == nil {
		if err == nil {
			log.Warn("repository worker disabled: Projects are created but no sync runs and Git credentials cannot be submitted")
		}
		return err
	}
	store.GitSubmissions = &core.GitCredentialSubmissions{Key: opened.SubmissionKey, IngressURL: opened.IngressURL}
	store.GitCredentialStore = credentialStoreManager{client: opened.CredentialStore}
	store.GitRefs = opened.Refs
	host, _ := os.Hostname()
	worker, err := repoworker.New(&repoworker.Config{
		Store: store, Executor: opened.Executor, Refs: opened.Refs, Key: opened.AuthorizationKey, Owner: "cloud:" + host,
		Slots: cfg.Slots, Lease: cfg.Lease, SyncTimeout: cfg.SyncTimeout, PublishTimeout: cfg.PublishTimeout, Log: log.Named("repository"),
	})
	if err != nil {
		return err
	}
	group.Add(1)
	go func() {
		defer group.Done()
		worker.Run(ctx)
	}()
	return nil
}
