// cloudctl is the restricted deployment management path, run with database operator credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repository"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return runArgs(context.Background(), os.Args[1:], os.Stdout)
}

type commandOptions struct {
	configFile, command, name, source, subject, display string
	residual                                            core.ResidualManagementRequest
}

func parseCommand(args []string) (commandOptions, error) {
	var opts commandOptions
	flags := flag.NewFlagSet("cloudctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.configFile, "config", "", "configuration file")
	flags.StringVar(&opts.command, "command", "", "migrate | bootstrap")
	flags.StringVar(&opts.name, "name", "", "tenant name")
	flags.StringVar(&opts.source, "source", "", "stable identity namespace")
	flags.StringVar(&opts.subject, "subject", "", "identity subject")
	flags.StringVar(&opts.display, "display-name", "", "display name")
	if err := flags.Parse(args); err != nil {
		return opts, invalidArguments()
	}
	if rest := flags.Args(); len(rest) != 0 {
		if rest[0] != "residual" || opts.command != "" || opts.name != "" || opts.source != "" || opts.subject != "" || opts.display != "" {
			return opts, invalidArguments()
		}
		request, err := parseResidual(rest[1:])
		if err != nil {
			return opts, err
		}
		opts.command, opts.residual = "residual", request
	}
	if opts.command != "migrate" && opts.command != "bootstrap" && (opts.command != "residual" || opts.residual.Action == "") {
		return opts, invalidArguments()
	}
	return opts, nil
}

func runArgs(ctx context.Context, args []string, output io.Writer) error {
	opts, e := parseCommand(args)
	if e != nil {
		return e
	}
	cfg, e := config.Load(opts.configFile)
	if e != nil {
		return errors.New("configuration_error")
	}
	db, e := repository.InitDB(ctx, cfg.Database)
	if e != nil {
		return errors.New("database_unavailable")
	}
	s, e := core.NewStore(db)
	if e != nil {
		if pool, poolErr := db.DB(); poolErr == nil {
			_ = pool.Close()
		}
		return errors.New("database_unavailable")
	}
	defer s.Pool.Close()
	var out core.Object
	switch opts.command {
	case "migrate":
		e = s.Migrate(ctx)
		out = core.Object{"migrated": e == nil}
	case "bootstrap":
		out, e = s.Bootstrap(ctx, opts.name, opts.source, opts.subject, opts.display)
	case "residual":
		return dispatchResidual(ctx, &opts.residual, output, s.ManageResidual)
	}
	if e != nil {
		return core.ErrorCode(e)
	}
	return json.NewEncoder(output).Encode(out)
}
