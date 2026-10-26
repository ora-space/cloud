package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	coreFixtureTemplatePrefix = "core_template_"
	coreFixtureCopyPrefix     = "core_run_"
)

var coreFixtureState struct {
	once     sync.Once
	template *coreFixtureTemplate
	err      error
}

// This mirrors integration's test-only helper: that package already imports core and its
// _test.go symbols cannot be shared without adding a separate callback-based support API.
type coreFixtureTemplate struct {
	admin       *sql.DB
	config      *pgx.ConnConfig
	name        string
	created     bool
	migrateTime time.Duration
	copies      atomic.Int64
	copyNanos   atomic.Int64
}

func coreFixturePool(t *testing.T, registerCleanup func(func() error)) *sql.DB {
	t.Helper()
	config := coreTestDatabaseConfig(t)
	coreFixtureState.once.Do(func() {
		coreFixtureState.template, coreFixtureState.err = newCoreFixtureTemplate(t.Context(), config)
	})
	if coreFixtureState.err != nil {
		t.Fatal(coreFixtureState.err)
	}
	template := coreFixtureState.template
	name := coreFixtureCopyPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	start := time.Now()
	_, err := template.admin.ExecContext(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{template.name}.Sanitize())
	if err != nil {
		t.Fatalf("copy core fixture template: %v", err)
	}
	template.copyNanos.Add(int64(time.Since(start)))
	template.copies.Add(1)
	copyConfig := template.config.Copy()
	copyConfig.Database = name
	pool := stdlib.OpenDB(*copyConfig)
	registerCleanup(func() error {
		poolErr := pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Drop only the database this fixture created, including connections reopened by a test.
		_, dropErr := template.admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		return errors.Join(poolErr, dropErr)
	})
	return pool
}

func coreTestDatabaseConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required; agent run DB tests must not skip under REQUIRE_POSTGRES")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	return config
}

func newCoreFixtureTemplate(ctx context.Context, config *pgx.ConnConfig) (_ *coreFixtureTemplate, err error) {
	template := &coreFixtureTemplate{
		admin: stdlib.OpenDB(*config),
		name:  coreFixtureTemplatePrefix + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, template.close())
		}
	}()
	var canCreate bool
	err = template.admin.QueryRowContext(ctx, "SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&canCreate)
	if err != nil {
		return nil, fmt.Errorf("check core template privileges: %w", err)
	}
	if !canCreate {
		return nil, errors.New("core fixture templates require CREATEDB for the TEST_DATABASE_URL role")
	}
	_, err = template.admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{template.name}.Sanitize()+" TEMPLATE template0")
	if err != nil {
		return nil, fmt.Errorf("create core template: %w", err)
	}
	template.created = true
	template.config = config.Copy()
	template.config.Database = template.name
	template.config.RuntimeParams["search_path"] = "core_seed_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	template.migrateTime, err = migrateCoreFixtureTemplate(ctx, template.config)
	if err != nil {
		return nil, fmt.Errorf("migrate core template: %w", err)
	}
	// A template must have no open connections before cloning. Sealing also prevents tests
	// from connecting to the source instead of their own copy.
	_, err = template.admin.ExecContext(ctx, "ALTER DATABASE "+pgx.Identifier{template.name}.Sanitize()+" ALLOW_CONNECTIONS false")
	if err != nil {
		return nil, fmt.Errorf("seal core template: %w", err)
	}
	return template, nil
}

func migrateCoreFixtureTemplate(ctx context.Context, config *pgx.ConnConfig) (elapsed time.Duration, err error) {
	pool := stdlib.OpenDB(*config)
	defer func() { err = errors.Join(err, pool.Close()) }()
	_, err = pool.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{config.RuntimeParams["search_path"]}.Sanitize())
	if err != nil {
		return 0, err
	}
	start := time.Now()
	err = (&Store{Pool: pool}).Migrate(ctx)
	return time.Since(start), err
}

func (f *coreFixtureTemplate) close() error {
	var dropErr error
	if f.created {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, dropErr = f.admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{f.name}.Sanitize()+" WITH (FORCE)")
	}
	return errors.Join(dropErr, f.admin.Close())
}

func closeCoreFixtureTemplate() error {
	template := coreFixtureState.template
	if template == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "CORE_FIXTURE_TEMPLATE migrations=1 migrate_seconds=%.9f copies=%d copy_seconds=%.9f\n",
		template.migrateTime.Seconds(), template.copies.Load(), time.Duration(template.copyNanos.Load()).Seconds())
	return template.close()
}
