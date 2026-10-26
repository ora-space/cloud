package integration

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

	"github.com/wanglongan587/cloud/internal/core"
)

var fixtureTemplateState struct {
	once  sync.Once
	value *fixtureTemplate
	err   error
}

// fixtureTemplate owns a sealed, migrated database. Only its administrator pool remains open;
// test pools connect exclusively to independent copies, never to the source database.
type fixtureTemplate struct {
	admin       *sql.DB
	config      *pgx.ConnConfig
	name        string
	created     bool
	migrateTime time.Duration
	copies      atomic.Int64
	copyNanos   atomic.Int64
}

func migratedTestSchema(t *testing.T, prefix string) (*sql.DB, *pgx.ConnConfig) {
	t.Helper()
	return migratedTestSchemaWithCleanup(t, prefix, func(cleanup func() error) {
		t.Cleanup(func() {
			if err := cleanup(); err != nil {
				t.Error(err)
			}
		})
	})
}

func migratedTestSchemaWithCleanup(t *testing.T, prefix string, registerCleanup func(func() error)) (*sql.DB, *pgx.ConnConfig) {
	t.Helper()
	config := testDatabaseConfig(t)
	fixtureTemplateState.once.Do(func() {
		fixtureTemplateState.value, fixtureTemplateState.err = newFixtureTemplate(t.Context(), config)
	})
	must(t, fixtureTemplateState.err)
	template := fixtureTemplateState.value
	name := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	start := time.Now()
	_, err := template.admin.ExecContext(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{template.name}.Sanitize())
	must(t, err)
	template.copyNanos.Add(int64(time.Since(start)))
	template.copies.Add(1)

	copyConfig := template.config.Copy()
	copyConfig.Database = name
	pool := stdlib.OpenDB(*copyConfig)
	registerCleanup(func() error {
		poolErr := pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Names come only from this creation, not from catalog scans. FORCE also releases
		// connections a fixture reopened against its own copy after its primary pool closed.
		_, dropErr := template.admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		return errors.Join(poolErr, dropErr)
	})
	return pool, copyConfig
}

func newFixtureTemplate(ctx context.Context, config *pgx.ConnConfig) (_ *fixtureTemplate, err error) {
	template := &fixtureTemplate{
		admin: stdlib.OpenDB(*config),
		name:  "test_template_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, template.close())
		}
	}()
	var canCreate bool
	err = template.admin.QueryRowContext(ctx, "SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&canCreate)
	if err != nil {
		return nil, fmt.Errorf("check integration template privileges: %w", err)
	}
	if !canCreate {
		return nil, errors.New("integration fixture templates require CREATEDB for the TEST_DATABASE_URL role")
	}
	_, err = template.admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{template.name}.Sanitize()+" TEMPLATE template0")
	if err != nil {
		return nil, fmt.Errorf("create integration template: %w", err)
	}
	template.created = true
	template.config = config.Copy()
	template.config.Database = template.name
	template.config.RuntimeParams["search_path"] = "test_seed_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	template.migrateTime, err = migrateFixtureTemplate(ctx, template.config)
	if err != nil {
		return nil, fmt.Errorf("migrate integration template: %w", err)
	}
	// PostgreSQL cannot clone a database with active connections. The migration pool is
	// closed before sealing, so no fixture can connect to or mutate this source.
	_, err = template.admin.ExecContext(ctx, "ALTER DATABASE "+pgx.Identifier{template.name}.Sanitize()+" ALLOW_CONNECTIONS false")
	if err != nil {
		return nil, fmt.Errorf("seal integration template: %w", err)
	}
	return template, nil
}

func migrateFixtureTemplate(ctx context.Context, config *pgx.ConnConfig) (elapsed time.Duration, err error) {
	pool := stdlib.OpenDB(*config)
	defer func() { err = errors.Join(err, pool.Close()) }()
	_, err = pool.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{config.RuntimeParams["search_path"]}.Sanitize())
	if err != nil {
		return 0, err
	}
	start := time.Now()
	err = (&core.Store{Pool: pool}).Migrate(ctx)
	return time.Since(start), err
}

func (f *fixtureTemplate) close() error {
	var dropErr error
	if f.created {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, dropErr = f.admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{f.name}.Sanitize()+" WITH (FORCE)")
	}
	return errors.Join(dropErr, f.admin.Close())
}

// closeFixtureTemplate runs after test cleanups and the serial controlled fixture's cleanup.
func closeFixtureTemplate() error {
	template := fixtureTemplateState.value
	if template == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "FIXTURE_TEMPLATE migrations=1 migrate_seconds=%.9f copies=%d copy_seconds=%.9f\n",
		template.migrateTime.Seconds(), template.copies.Load(), time.Duration(template.copyNanos.Load()).Seconds())
	return template.close()
}
