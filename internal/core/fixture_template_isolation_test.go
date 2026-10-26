package core

import (
	"fmt"
	"testing"
)

func TestCoreFixtureTemplateCopiesKeepDataAndSchemaIsolated(t *testing.T) {
	first := dispatcherDB(t)
	second := dispatcherDB(t)
	var firstDatabase, secondDatabase string
	if err := first.Pool.QueryRow("SELECT current_database()").Scan(&firstDatabase); err != nil {
		t.Fatal(err)
	}
	if err := second.Pool.QueryRow("SELECT current_database()").Scan(&secondDatabase); err != nil {
		t.Fatal(err)
	}
	if firstDatabase == secondDatabase {
		t.Fatal("fixtures received the same database")
	}
	var migrations int
	if err := first.Pool.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations == 0 {
		t.Fatal("template copy has no migration history")
	}
	for _, query := range []string{
		"CREATE TABLE core_fixture_marker(value text NOT NULL)",
		"INSERT INTO core_fixture_marker VALUES ('first only')",
		"DELETE FROM schema_migrations WHERE version=(SELECT min(version) FROM schema_migrations)",
	} {
		if _, err := first.Pool.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	// A copy created after a mutation must also start from the immutable source.
	third := dispatcherDB(t)
	for _, store := range []*Store{second, third} {
		var absent bool
		var count int
		if err := store.Pool.QueryRow("SELECT to_regclass('core_fixture_marker') IS NULL").Scan(&absent); err != nil {
			t.Fatal(err)
		}
		if err := store.Pool.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if !absent || count != migrations {
			t.Fatalf("fixture mutation escaped its copy: marker absent=%v, migrations=%d want %d", absent, count, migrations)
		}
		if err := store.CheckSchema(t.Context()); err != nil {
			t.Fatalf("copied schema must pass product migration checks: %v", err)
		}
	}
	var allowConnections bool
	if err := second.Pool.QueryRow("SELECT datallowconn FROM pg_database WHERE datname=$1", coreFixtureState.template.name).Scan(&allowConnections); err != nil {
		t.Fatal(err)
	}
	if allowConnections {
		t.Fatal("source template must reject test connections")
	}
}

func TestCoreFixtureTemplateParallelCopies(t *testing.T) {
	for i := range 2 {
		t.Run(fmt.Sprintf("copy_%d", i), func(t *testing.T) {
			// The shared source is sealed; identical DDL and distinct data belong to separate copies.
			t.Parallel()
			store := dispatcherDB(t)
			if _, err := store.Pool.Exec("CREATE TABLE core_parallel_marker(value integer NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Pool.Exec("INSERT INTO core_parallel_marker VALUES ($1)", i); err != nil {
				t.Fatal(err)
			}
			var count, value int
			if err := store.Pool.QueryRow("SELECT count(*),min(value) FROM core_parallel_marker").Scan(&count, &value); err != nil {
				t.Fatal(err)
			}
			if count != 1 || value != i {
				t.Fatalf("parallel fixture rows = %d/%d, want 1/%d", count, value, i)
			}
		})
	}
}
