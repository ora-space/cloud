// Command git-server-preset prepares and checks Git Server contract fixtures for acceptance. It
// writes the repository volume and the Cloud database directly and ships only in the development
// (devsetup) image, never in the service image.
//
//	git-server-preset [-config path] snapshot  --project <id> (--commit <oid> | --synthetic <n>)
//	git-server-preset [-config path] ref       --project <id> --ref <name> --commit <oid>
//	git-server-preset [-config path] operation --project <id> --type sync|publish --state <state> [--error-code <code>]
//	git-server-preset [-config path] check     --project <id>
//
// It prints one JSON line and exits 0 on success, 1 when check finds violations or a command
// fails, and 2 on a usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitpreset"
	"github.com/wanglongan587/cloud/internal/gitserver"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// command is one parsed subcommand, ready to run against a Preset.
type command func(context.Context, *gitpreset.Preset) (gitpreset.Result, error)

func run(args []string, out io.Writer) int {
	emit := func(v any) { _ = json.NewEncoder(out).Encode(v) }
	global := flag.NewFlagSet("git-server-preset", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	path := global.String("config", "", "Cloud database configuration")
	if err := global.Parse(args); err != nil || global.NArg() == 0 {
		emit(map[string]any{"ok": false, "error": "usage: git-server-preset [-config path] snapshot|ref|operation|check --project <id> ..."})
		return 2
	}
	cmd, err := parse(global.Arg(0), global.Args()[1:])
	if err != nil {
		emit(map[string]any{"ok": false, "error": err.Error()})
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	preset, closeDB, err := open(ctx, *path)
	if err != nil {
		emit(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	defer closeDB()
	result, err := cmd(ctx, preset)
	var usage *gitpreset.UsageError
	switch {
	case errors.As(err, &usage):
		emit(map[string]any{"ok": false, "error": err.Error()})
		return 2
	case err != nil:
		emit(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	emit(result)
	if ok, _ := result["ok"].(bool); !ok {
		return 1
	}
	return 0
}

// parse reads one subcommand's flags; unknown flags, positional leftovers and missing values are
// usage errors before anything is opened.
func parse(name string, args []string) (command, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "Project ID")
	var cmd command
	switch name {
	case "snapshot":
		commit := fs.String("commit", "", "existing commit to snapshot")
		synthetic := fs.Int("synthetic", 0, "create a history of n commits and snapshot its tip")
		cmd = func(ctx context.Context, p *gitpreset.Preset) (gitpreset.Result, error) {
			return p.Snapshot(ctx, *project, *commit, *synthetic)
		}
	case "ref":
		ref := fs.String("ref", "", "full ref name in a contract namespace")
		commit := fs.String("commit", "", "existing commit")
		cmd = func(ctx context.Context, p *gitpreset.Preset) (gitpreset.Result, error) {
			return p.Ref(ctx, *project, *ref, *commit)
		}
	case "operation":
		kind := fs.String("type", "", "sync or publish")
		state := fs.String("state", "", "Operation state")
		code := fs.String("error-code", "", "error code")
		cmd = func(ctx context.Context, p *gitpreset.Preset) (gitpreset.Result, error) {
			return p.Operation(ctx, *project, *kind, *state, *code)
		}
	case "check":
		cmd = func(ctx context.Context, p *gitpreset.Preset) (gitpreset.Result, error) {
			return p.Check(ctx, *project)
		}
	default:
		return nil, fmt.Errorf("unknown subcommand %q", name)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 || *project == "" {
		return nil, errors.New(name + " needs --project <id> and no positional arguments")
	}
	return cmd, nil
}

// open connects to the migrated Cloud database and locates the Git Server's repository volume
// from the same environment the Git Server reads.
func open(ctx context.Context, path string) (*gitpreset.Preset, func(), error) {
	deployment, err := gitserver.LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	db, err := repository.InitDB(ctx, cfg.Database)
	if err != nil {
		return nil, nil, err
	}
	store, err := core.NewStore(db)
	if err != nil {
		return nil, nil, err
	}
	if err := store.CheckSchema(ctx); err != nil {
		_ = store.Pool.Close()
		return nil, nil, err
	}
	return &gitpreset.Preset{DB: store.Pool, Root: deployment.RepositoryRoot, Git: deployment.GitProgram}, func() { _ = store.Pool.Close() }, nil
}
