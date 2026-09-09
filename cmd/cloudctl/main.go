// cloudctl is the restricted deployment management path, run with database operator credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	configFile := flag.String("config", "", "configuration file")
	command := flag.String("command", "", "migrate | bootstrap | credential-ref")
	name := flag.String("name", "", "tenant name")
	source := flag.String("source", "", "stable identity namespace")
	subject := flag.String("subject", "", "identity subject")
	display := flag.String("display-name", "", "display name")
	tenant := flag.String("tenant", "", "tenant UUID")
	owner := flag.String("owner", "", "owner UUID")
	secret := flag.String("secret-ref", "", "infrastructure secret reference, never a secret value")
	flag.Parse()
	cfg, e := config.Load(*configFile)
	if e != nil {
		return e
	}
	db, e := repository.InitDB(context.Background(), cfg.Database)
	if e != nil {
		return e
	}
	s, e := core.NewStore(db)
	if e != nil {
		return e
	}
	defer s.Pool.Close()
	ctx := context.Background()
	var out core.Object
	switch *command {
	case "migrate":
		e = s.Migrate(ctx)
		out = core.Object{"migrated": e == nil}
	case "bootstrap":
		out, e = s.Bootstrap(ctx, *name, *source, *subject, *display)
	case "credential-ref":
		out, e = s.ConfigureCredential(ctx, *tenant, *owner, *secret)
	default:
		return fmt.Errorf("unknown command; choose migrate, bootstrap, credential-ref")
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
