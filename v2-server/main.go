//go:build linux

// Command v2-server is Cloud v2's temporary single-process backend; see README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/boot"
	"github.com/wanglongan587/cloud/v2-server/internal/completiontool"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/idaas"
	loginpkg "github.com/wanglongan587/cloud/v2-server/internal/login"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/preparedassets"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/requestmetrics"
	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
	"github.com/wanglongan587/cloud/v2-server/internal/workflowhost"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacereclaim"
	"golang.org/x/sys/unix"
)

// defaultDatabase is the database v2-server uses on the repository's compose PostgreSQL.
const defaultDatabase = "postgres://ora:ora-local@127.0.0.1:55432/ora_v2?sslmode=disable"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "reclaims" || os.Args[1] == "discard-kept") {
		command := reclaimsCommand
		if os.Args[1] == "discard-kept" {
			command = discardKeptCommand
		}
		if e := command(os.Args[2:], os.Stdout); e != nil {
			slog.Error(os.Args[1], "error", e)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "admin-invite" {
		if e := adminInvite(os.Args[2:], os.Stdout); e != nil {
			slog.Error("admin-invite", "error", e)
			os.Exit(1)
		}
		return
	}
	addr := flag.String("addr", envOr("V2_ADDR", "127.0.0.1:8090"), "listen address")
	dbURL := flag.String("db", envOr("V2_DATABASE_URL", defaultDatabase), "PostgreSQL URL")
	cleanup := flag.Bool("cleanup", false, "stop and remove owned conversation directories; retain repository mirrors and database history")
	fixture := flag.Bool("fixture-check", false, "run the real Node fixture through the public execution service, then exit")
	testFixtures := flag.Bool("test-fixtures", false, "include deterministic fixture models for end-to-end tests")
	flag.Parse()

	if e := run(*addr, *dbURL, *cleanup, *fixture, *testFixtures); e != nil {
		slog.Error("v2-server", "error", e)
		os.Exit(1)
	}
}

func run(addr, dbURL string, cleanup, fixture, testFixtures bool) error {
	return runWithListener(addr, dbURL, cleanup, fixture, testFixtures, (*http.Server).ListenAndServe)
}

func runWithListener(addr, dbURL string, cleanup, fixture, testFixtures bool, listen func(*http.Server) error) (resultErr error) {
	login, e := parseLogin(os.Getenv("V2_LOGIN"), fixture || testFixtures)
	if e != nil {
		return e
	}
	publicURL, e := parsePublicURL(os.Getenv("V2_PUBLIC_URL"))
	if e != nil {
		return e
	}
	// -cleanup only removes Node directories and serves nothing, so it needs no login configuration.
	var redirects map[string]loginpkg.Provider
	if !cleanup {
		if redirects, e = loginRedirects(login, publicURL, os.Getenv); e != nil {
			return e
		}
	}
	slog.Info("login methods", "V2_LOGIN", strings.Join(login, ","))
	if c, ok := redirects[server.LoginProviderHuaweiIDaaS].(*idaas.Client); ok {
		authorize, token, userinfo := c.Endpoints()
		// The callback is what the IDaaS application must have registered, character for character.
		slog.Info("huawei-idaas", "callback", publicURL.String()+"/auth/callback/"+server.LoginProviderHuaweiIDaaS, "authorize", authorize, "token", token, "userinfo", userinfo)
	}
	if login.has(server.LoginProviderEmail) && publicURL != nil && publicURL.Scheme == "https" {
		slog.Warn("email login is on at an https address: knowing a member's address is enough to sign in as them; a deployment sets V2_LOGIN without email")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base, e := dataDirectory()
	if e != nil {
		return e
	}
	lock, e := lockDirectory(base)
	if e != nil {
		return e
	}
	defer lock.Close()
	if cleanup {
		return agentruntime.Cleanup(ctx, filepath.Join(base, "nodes"))
	}

	limit, e := strconv.ParseInt(envOr("V2_DELIVERY_MAX_OBJECT_BYTES", strconv.Itoa(workspacefs.DefaultDeliveryObjectBytes)), 10, 64)
	if e != nil || limit <= 0 {
		return errors.New("V2_DELIVERY_MAX_OBJECT_BYTES must be a positive integer")
	}
	// The data-root lock excludes delivery writers from the previous process.
	scratch := filepath.Join(base, "delivery-scratch")
	if e := os.RemoveAll(scratch); e != nil {
		return fmt.Errorf("clear delivery scratch: %w", e)
	}
	if e := os.Mkdir(scratch, 0700); e != nil {
		return fmt.Errorf("create delivery scratch: %w", e)
	}
	delivery, e := workspacefs.NewDeliveryFS(scratch, limit)
	if e != nil {
		return e
	}
	maxPatch, e := deliveryPatchLimit(envOr("V2_DELIVERY_MAX_PATCH_BYTES", strconv.Itoa(server.DefaultMaxPatchBytes)))
	if e != nil {
		return e
	}

	db, e := store.Open(ctx, dbURL)
	if e != nil {
		return e
	}
	for _, m := range db.AppliedMigrations() {
		slog.Info("applied database migration", "version", m.Version, "name", m.Name)
	}
	if e := db.ClaimProfile(ctx, databaseProfile(login)); e != nil {
		db.Close()
		return e
	}
	db.DisableOfficialMarket(envDefault("V2_PLUGIN_MARKETPLACE_URL", "https://github.com/ora-space/marketplace") == "")
	var preset store.Preset
	if login.has(server.LoginProviderEmail) {
		preset, e = store.LoadPreset(envOr("V2_USERS_FILE", "users.json"))
		if e != nil {
			db.Close()
			return e
		}
		if e := db.SeedUsers(ctx, preset); e != nil {
			db.Close()
			return e
		}
		if e := db.MigrateStarterSkillNames(ctx); e != nil {
			db.Close()
			return e
		}
		if e := db.RetireLegacyDemoUser(ctx); e != nil {
			db.Close()
			return e
		}
		if e := db.RetireUsers(ctx, preset.Retired); e != nil {
			db.Close()
			return e
		}
	}
	if e := db.EndDeletedSquadMembers(ctx); e != nil {
		db.Close()
		return e
	}
	// The demo project is owned by the first preset owner; it seeds shared content, not a login.
	bootstrap := ""
	if len(preset.Users) > 0 {
		bootstrap = preset.Users[0].Email
	}
	for _, u := range preset.Users {
		if u.Role == store.RoleOwner {
			bootstrap = u.Email
			break
		}
	}
	closeDatabase := true
	defer func() {
		if closeDatabase {
			db.Close()
		}
	}()
	projects, e := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(base, "repositories"), CredentialKey: os.Getenv("V2_MARKET_CREDENTIAL_KEY")})
	if e != nil {
		return e
	}
	if e := db.ClearStaleSyncs(ctx); e != nil {
		return e
	}
	models, e := executionruntime.ModelsFromEnvironment(os.Getenv)
	if e != nil {
		return e
	}
	if fixture || testFixtures {
		models = executionruntime.WithFixtureModels(models)
	}
	var driver *agentruntime.Driver
	if assets := os.Getenv("V2_NODE_ASSETS_DIR"); assets != "" && len(models) > 0 {
		if e := preparedassets.Verify(assets); e != nil {
			return e
		}
		if e := checkReleaseNodeDataDirectory(assets, base); e != nil {
			return e
		}
		gitDir, e := exec.CommandContext(ctx, "git", "--exec-path").Output()
		if e != nil {
			return errors.New("git executable unavailable")
		}
		bindings := map[string]nodeservices.Binding{}
		for id, choice := range models {
			bindings[id] = choice.Binding
		}
		driver, e = agentruntime.New(agentruntime.Options{Root: filepath.Join(base, "nodes"), AssetsDirectory: assets, GitBackend: filepath.Join(strings.TrimSpace(string(gitDir)), "git-http-backend"), Bindings: bindings, ObserveProvider: func(ref execution.Ref, binding nodeservices.Binding) nodeservices.Provider {
			return requestmetrics.Wrap(db, ref, binding)
		}, ResolveProject: func(c context.Context, scope execution.Scope, id string) (agentruntime.Project, error) {
			p, e := db.GetProject(c, scope, id)
			return agentruntime.Project{MirrorPath: p.MirrorPath, Commit: p.Commit, SnapshotRef: p.SnapshotRef}, e
		}, ReadMCPTarget: db.ReadSessionMCPTarget, UserSkills: func(c context.Context, scope execution.Scope) ([]agentruntime.UserSkill, error) {
			items, e := db.UserSkillDocuments(c, scope.TenantID, scope.UserID)
			if e != nil {
				return nil, e
			}
			skills := make([]agentruntime.UserSkill, len(items))
			for i, item := range items {
				skills[i] = agentruntime.UserSkill{Name: item.Name, Description: item.Description, Content: item.Content}
			}
			return skills, nil
		}, RecordWorkspace: func(c context.Context, id string, at agentruntime.Checkout) error {
			return db.RecordWorkspace(c, id, store.WorkspaceLocation{Path: at.Path, BaseCommit: at.BaseCommit, CloneExecution: at.CloneExecution})
		}, ForgetCheckout: db.ForgetCloneExecution, RecordHostExecution: db.RecordHostExecution, EndHostExecution: db.EndHostExecution})
		if e != nil {
			return e
		}
	} else if e := agentruntime.Recover(ctx, filepath.Join(base, "nodes")); e != nil {
		return e
	}
	var agent executionruntime.AgentDriver
	if driver != nil {
		agent = driver
		db.SetMCPChangeObserver(driver.MCPChanged)
	}
	runtime, e := executionruntime.New(ctx, executionruntime.Config{Store: db, Models: models, Agent: agent, Delivery: delivery})
	if e != nil {
		if driver != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			stopErr := driver.Close(shutdown)
			cancel()
			e = errors.Join(e, stopErr)
		}
		return e
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runtime.Close(shutdown); err != nil {
			// A worker may still be retaining its terminal result. Keep its database
			// handle until the failed process exits; next startup performs recovery.
			closeDatabase = false
			resultErr = errors.Join(resultErr, fmt.Errorf("execution shutdown unconfirmed: %w", err))
		}
	}()
	// Recovery above ended the previous process's sessions; show their conversations read-only now.
	if e := db.InterruptEndedChatSessions(ctx); e != nil {
		return e
	}
	var scope execution.Scope
	var demo store.Project
	if login.has(server.LoginProviderEmail) {
		scope, demo, e = prepareDemo(ctx, db, projects, bootstrap)
		if e != nil {
			return e
		}
		if fixture {
			if driver == nil {
				return errors.New("fixture-check requires V2_NODE_ASSETS_DIR; run task v2:node:prepare first")
			}
			return checkFixture(ctx, runtime, scope, demo.ID, filepath.Join(base, "nodes"))
		}
	}
	marketValue := envDefault("V2_PLUGIN_MARKETPLACE_URL", "https://github.com/ora-space/marketplace")
	var refreshOfficial func(context.Context) error
	if marketValue != "" {
		marketURL, e := pluginmarket.NormalizeSourceURL(marketValue)
		if e != nil {
			return e
		}
		marketBranch := envOr("V2_PLUGIN_MARKETPLACE_BRANCH", "main")
		if !pluginmarket.ValidBranch(marketBranch) {
			return fmt.Errorf("V2_PLUGIN_MARKETPLACE_BRANCH is not a supported Git branch")
		}
		marketInterval, e := time.ParseDuration(envOr("V2_PLUGIN_MARKETPLACE_INTERVAL", "5m"))
		if e != nil || marketInterval <= 0 {
			return fmt.Errorf("V2_PLUGIN_MARKETPLACE_INTERVAL must be a positive duration")
		}
		market := &pluginmarket.Syncer{
			Source: pluginmarket.Source{
				Namespace: "official", URL: marketURL, Branch: marketBranch,
			},
			WorkDir: filepath.Join(base, "marketplace"),
		}
		marketCtx, cancelMarket := context.WithCancel(ctx)
		marketDone := make(chan struct{})
		go func() {
			defer close(marketDone)
			if e := market.Sync(marketCtx, db, time.Now()); e != nil {
				slog.Warn("initial plugin marketplace sync failed; retaining previous snapshot", "error", e)
			}
			market.Loop(marketCtx, marketInterval, db)
		}()
		defer func() {
			cancelMarket()
			<-marketDone
		}()

		refreshOfficial = func(ctx context.Context) error { return market.Refresh(ctx, db, time.Now()) }
	}
	skillCtx, cancelSkills := context.WithCancel(ctx)
	skillDone := make(chan struct{})
	go func() { defer close(skillDone); runSkillReconciler(skillCtx, db, driver) }()
	defer func() { cancelSkills(); <-skillDone }()
	// Startup recovery settles what the previous process left live before serving (boot.Recover
	// owns the order). Runs owned by a task — handling runs and decomposition runs — never outlive
	// the process: their owners cancel them first, except a decomposition run at a submitted review,
	// which the review recovery finishes and its decomposition adopts after the sweep. Of the other
	// workflow runs, the sweep fails what was executing except an interrupted Iteration or pending
	// LoopExit cleanup it can absorb and resume under its frozen starter. Those runs and runs frozen
	// at a review form outlive the process. Hosts stop observing turns before the execution runtime closes
	// (defers run LIFO).
	// agentruntime.New or Recover above has stopped the Nodes of earlier processes, and the
	// execution runtime has ended their executions: no credential of theirs is left to honor
	// (package completiontool).
	completionTool := completiontool.NewRegistry(db, runtime.HostExecutionEnding)
	completionServer, e := completiontool.Serve(completionTool)
	if e != nil {
		return fmt.Errorf("serve the Agent completion tool: %w", e)
	}
	defer completionServer.Close()
	workflowRuns, e := workflowhost.NewHost(workflowhost.Config{Store: db, Execution: runtime, Catalog: runtime, CompletionTool: completionTool, CheckHostTools: runtime.CheckHostTools,
		NoAgentRuntime: driver == nil})
	if e != nil {
		return e
	}
	defer workflowRuns.Close()
	taskWorkflows := taskworkflow.New(db, taskWorkflowRunner{workflowRuns})
	defer taskWorkflows.Close()
	taskRuns := taskrun.New(db, runtime, runtime, nil)
	defer taskRuns.Close()
	decompositions := taskdecomp.New(db, workflowRunStarter{workflowRuns}, nil)
	defer decompositions.Close()
	// Deleted conversations' checkouts; without an Agent runtime there are none to reclaim.
	hosts := boot.Hosts{TaskWorkflows: taskWorkflows, Decompositions: decompositions, WorkflowRuns: workflowRuns, TaskRuns: taskRuns, Deliveries: runtime}
	var reclaims *workspacereclaim.Reclaimer
	if driver != nil {
		reclaims = workspacereclaim.New(db, delivery, driver)
		// Stops background removals before the runtime and database close (defers run LIFO);
		// a removal stopped this way stays deleting for the next start.
		defer reclaims.Close()
		hosts.WorkspaceReclaims = reclaims
	}
	if e := boot.Recover(ctx, hosts); e != nil {
		return e
	}
	if login.has(server.LoginProviderEmail) && driver != nil {
		go driver.WarmDemo(ctx, demo.ID, agentruntime.Project{MirrorPath: demo.MirrorPath, Commit: demo.Commit, SnapshotRef: demo.SnapshotRef})
	}
	options := withWorkflowDomain(server.Options{
		RefreshOfficial:    refreshOfficial,
		SessionTTL:         7 * 24 * time.Hour,
		LoginProviders:     login,
		LoginRedirects:     redirects,
		PublicURL:          publicURL,
		Projects:           projects,
		Execution:          runtime,
		Catalog:            runtime,
		TaskRuns:           taskRuns,
		TaskDecompositions: decompositions,
		TaskWorkflows:      taskWorkflows,
		Deliveries:         delivery,
		MaxPatchBytes:      maxPatch,
		// The signal context ends with the process, before the database pool is closed.
		Context: ctx,
	}, workflowRuns)
	if driver != nil {
		options.SessionMCP = driver
		options.WorkspaceReclaims = reclaims
	}
	coordinator, e := server.NewScheduleCoordinator(db, options, time.Now)
	if e != nil {
		return e
	}
	schedules, e := scheduler.NewHost(db, coordinator, options.Catalog)
	if e != nil {
		return e
	}
	scheduleCtx, cancelSchedules := context.WithCancel(ctx)
	ticks := time.NewTicker(time.Second)
	scheduleDone := make(chan error, 1)
	go func() { scheduleDone <- schedules.Run(scheduleCtx, ticks.C) }()
	defer func() {
		cancelSchedules()
		ticks.Stop()
		<-scheduleDone // Join scans and admitted HTTP commands before closing the execution runtime and database.
	}()
	<-schedules.Ready()
	options.Schedules = schedules
	handler, e := server.WithFrontend(server.New(db, options), os.Getenv("V2_FRONTEND_DIR"))
	if e != nil {
		return e
	}
	srv := newHTTPServer(ctx, addr, handler)
	errs := make(chan error, 1)
	go func() { errs <- listen(srv) }()
	slog.Info("v2-server listening", "addr", addr)

	select {
	case e := <-errs:
		return e
	case <-ctx.Done():
	}
	// Cancel periodic scans and in-flight schedule commands before HTTP's 5s drain window.
	cancelSchedules()
	ticks.Stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := srv.Shutdown(shutdown); e != nil {
		// Cancelled handlers can still be blocked writing to a slow client. Release
		// their connections before closing the execution service and database.
		return errors.Join(e, srv.Close())
	}
	if e := <-errs; !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	return nil
}

func dataDirectory() (string, error) {
	root := os.Getenv("V2_DATA_DIR")
	if root == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		root = filepath.Join(home, ".ora-v2")
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return "", e
	}
	return root, os.MkdirAll(root, 0700)
}

// A release-built Node refuses its data below a directory that group or other users can write
// (debug builds skip that check). Without this, every conversation fails with agent_prepare_failed
// and nothing names the directory.
func checkReleaseNodeDataDirectory(assets, root string) error {
	var manifest struct {
		Profile string `json:"profile"`
	}
	raw, e := os.ReadFile(filepath.Join(assets, "assets.json"))
	if e != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Profile != "release" {
		return nil
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		info, e := os.Stat(dir)
		if e != nil {
			return e
		}
		if mode := info.Mode().Perm(); mode&0o022 != 0 {
			return fmt.Errorf("data directory %s: %s is writable by group or others (mode %04o), which a release-built Node refuses; run chmod go-w on it or choose another V2_DATA_DIR", root, dir, mode)
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

// One v2 process owns the data directory; cleanup cannot race an active server.
func lockDirectory(root string) (*os.File, error) {
	f, e := os.OpenFile(filepath.Join(root, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("v2 data directory is already in use")
	}
	return f, nil
}

// prepareDemo seeds the same real project used by browser selectors and the fixture command.
// Its temporary session only acts for one preset account; no token reaches a page.
func prepareDemo(ctx context.Context, db *store.Store, projects *projectrepos.Service, email string) (execution.Scope, store.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	user, e := db.DemoMemberByEmail(ctx, email)
	if e != nil {
		return execution.Scope{}, store.Project{}, e
	}
	token, e := db.StartSession(ctx, user.ID, time.Hour)
	if e != nil {
		return execution.Scope{}, store.Project{}, e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.Logout(cleanup, token); err != nil {
			slog.Warn("remove demo bootstrap session", "error", err)
		}
	}()
	user, e = db.SessionUser(ctx, token)
	if e != nil {
		return execution.Scope{}, store.Project{}, e
	}
	tenants, e := db.Tenants(ctx, user.ID)
	if e != nil {
		return execution.Scope{}, store.Project{}, e
	}
	for _, tenant := range tenants {
		if tenant.Name == store.DemoTenant {
			scope := execution.Scope{TenantID: tenant.ID, UserID: user.ID}
			project, err := projects.SeedDemo(ctx, scope)
			return scope, project, err
		}
	}
	return execution.Scope{}, store.Project{}, errors.New("demo tenant unavailable")
}

func checkFixture(ctx context.Context, runtime *executionruntime.Service, scope execution.Scope, projectID, nodeRoot string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	before, e := fixtureMarkers(nodeRoot)
	if e != nil {
		return e
	}
	started := time.Now()
	caller := execution.Caller{Kind: execution.CallerChat, ID: fmt.Sprintf("fixture-%d", started.UnixNano()), AttemptID: "first"}
	ref, e := runtime.Start(ctx, scope, execution.StartRequest{Caller: caller, Target: execution.Target{ProjectID: projectID}, ExecutorID: "opencode:fixture", Prompt: execution.TextPrompt("Inspect README and save the fixture result.")})
	if e != nil {
		return e
	}
	stream, e := runtime.Subscribe(ctx, scope, ref.SessionID, 0)
	if e != nil {
		return e
	}
	defer stream.Close()
	for {
		event, e := stream.Next(ctx)
		if e != nil {
			return e
		}
		if event.Kind == execution.EventTurnEnded && event.Ref == ref {
			if event.Result == nil || event.Result.Status != execution.StatusCompleted || !event.Result.AnswerComplete || event.Result.Answer != nodeservices.FixtureAnswer {
				return errors.New("fixture did not complete with the real shell-tool answer")
			}
			break
		}
	}
	if e := runtime.End(ctx, scope, ref.SessionID); e != nil {
		return e
	}
	turn, e := runtime.ReadAttempt(ctx, scope, caller)
	if e != nil {
		return e
	}
	if turn.Result == nil || turn.Result.Answer != nodeservices.FixtureAnswer {
		return errors.New("fixture result unavailable after End")
	}
	after, e := fixtureMarkers(nodeRoot)
	if e != nil {
		return e
	}
	verified := false
	for path, value := range after {
		if _, existed := before[path]; !existed && value == nodeservices.FixtureMarker {
			verified = true
		}
	}
	if !verified {
		return errors.New("fixture shell tool did not produce a new verified checkout marker")
	}
	slog.Info("real Node fixture passed through execution.Service", "session", ref.SessionID, "turn", ref.TurnID, "elapsed", time.Since(started))
	return nil
}

func fixtureMarkers(root string) (map[string]string, error) {
	markers := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && entry.Name() == "ora-v2-result.txt" {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 4096 {
				return errors.New("oversized fixture marker")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			markers[path] = string(data)
		}
		return nil
	})
	return markers, err
}

// deliveryPatchLimit parses V2_DELIVERY_MAX_PATCH_BYTES: a positive byte count no larger than
// server.MaxPatchBytesCeiling, since every concurrent download may hold that much in memory.
func deliveryPatchLimit(value string) (int, error) {
	n, e := strconv.Atoi(value)
	if e != nil || n <= 0 || n > server.MaxPatchBytesCeiling {
		return 0, fmt.Errorf("V2_DELIVERY_MAX_PATCH_BYTES must be a positive integer of at most %d", server.MaxPatchBytesCeiling)
	}
	return n, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// workflowRunStarter lets task decompositions start workflow runs as a person and watch them end.
// Start translates the host's refusals into the decomposition's own errors, so the decomposition
// records the reason in the page's words; the host's error stays in the chain for the HTTP layer.
type workflowRunStarter struct{ host *workflowhost.Host }

func (s workflowRunStarter) Start(ctx context.Context, tenant, user, runID string) error {
	_, e := s.host.StartRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, runID)
	var missingSkill *server.WorkflowSkillNotFound
	var invalidInput *server.WorkflowRunInputInvalid
	switch {
	case errors.As(e, &missingSkill):
		return errors.Join(&taskdecomp.WorkflowSkillNotFound{
			NodeID:    missingSkill.NodeID,
			NodeTitle: missingSkill.NodeTitle,
			SkillID:   missingSkill.SkillID,
		}, e)
	case errors.Is(e, server.ErrWorkflowSkillNotFound):
		return errors.Join(taskdecomp.ErrWorkflowSkillNotFound, e)
	case errors.Is(e, server.ErrWorkflowRoleNotFound):
		return errors.Join(taskdecomp.ErrWorkflowRoleNotFound, e)
	case errors.As(e, &invalidInput):
		return errors.Join(&taskdecomp.WorkflowInputInvalid{Variable: invalidInput.Variable, Reason: invalidInput.Reason}, e)
	case errors.Is(e, server.ErrWorkflowCompletionToolUnavailable):
		return errors.Join(taskdecomp.ErrWorkflowCompletionToolUnavailable, e)
	case errors.Is(e, server.ErrWorkflowRunNotStartable):
		return errors.Join(taskdecomp.ErrWorkflowRunNotStartable, e)
	}
	return e
}

// Cancel stops the workflow run through the host, withdrawing a refused start or halting an
// already-dispatched Agent turn rather than only updating the database row.
func (s workflowRunStarter) Cancel(ctx context.Context, tenant, user, id string) error {
	_, err := s.host.CancelRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id)
	return err
}

func (s workflowRunStarter) Subscribe(runID string) (<-chan struct{}, func()) {
	return s.host.Hub().Subscribe(runID)
}

// taskWorkflowRunner adapts the existing host without changing workflow execution semantics. It
// passes the host's errors through unchanged: the task workflow records the refusal as the host
// worded it and the HTTP layer maps the host's sentinels, so the decomposition's translation does
// not apply here.
type taskWorkflowRunner struct{ host *workflowhost.Host }

func (s taskWorkflowRunner) Start(ctx context.Context, tenant, user, id string) error {
	_, err := s.host.StartRun(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id)
	return err
}

func (s taskWorkflowRunner) Cancel(ctx context.Context, tenant, user, id string) error {
	return workflowRunStarter{s.host}.Cancel(ctx, tenant, user, id)
}

func (s taskWorkflowRunner) Subscribe(runID string) (<-chan struct{}, func()) {
	return workflowRunStarter{s.host}.Subscribe(runID)
}

func (s taskWorkflowRunner) Prepare(ctx context.Context, tenant, user, id string, input *string, values map[string]json.RawMessage) error {
	_, err := s.host.UpdateRunInput(ctx, server.WorkflowRunScope{TenantID: tenant, UserID: user}, id, input, values)
	return err
}
