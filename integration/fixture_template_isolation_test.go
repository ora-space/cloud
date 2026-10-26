package integration

import (
	"database/sql"
	"fmt"
	"testing"
)

func TestFixtureTemplateCopiesKeepDataAndSchemaIsolated(t *testing.T) {
	first, firstConfig := migratedTestSchema(t, "test_copy_first_")
	second, secondConfig := migratedTestSchema(t, "test_copy_second_")
	if firstConfig.Database == secondConfig.Database {
		t.Fatal("fixtures received the same database")
	}
	for _, pair := range []struct {
		pool *sql.DB
		name string
	}{{first, firstConfig.Database}, {second, secondConfig.Database}} {
		var actual string
		must(t, pair.pool.QueryRow("SELECT current_database()").Scan(&actual))
		if actual != pair.name {
			t.Fatalf("pool connects to %q, want its own copy %q", actual, pair.name)
		}
	}
	var migrations int
	must(t, first.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations))
	if migrations == 0 {
		t.Fatal("template copy has no migration history")
	}
	_, err := first.Exec("CREATE TABLE fixture_copy_marker(value text NOT NULL)")
	must(t, err)
	_, err = first.Exec("INSERT INTO fixture_copy_marker VALUES ('first only')")
	must(t, err)
	_, err = first.Exec("DELETE FROM schema_migrations WHERE version=(SELECT min(version) FROM schema_migrations)")
	must(t, err)

	// A later copy also proves the first fixture never mutated the sealed source.
	third, _ := migratedTestSchema(t, "test_copy_later_")
	for _, pool := range []*sql.DB{second, third} {
		var absent bool
		var count int
		must(t, pool.QueryRow("SELECT to_regclass('fixture_copy_marker') IS NULL").Scan(&absent))
		must(t, pool.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count))
		if !absent || count != migrations {
			t.Fatalf("fixture mutation escaped its copy: marker absent=%v, migrations=%d want %d", absent, count, migrations)
		}
	}
	// Migration-specific fixtures must retain the empty-schema route.
	empty, _ := testSchema(t, "test_copy_empty_")
	var emptyHistory bool
	must(t, empty.QueryRow("SELECT to_regclass('schema_migrations') IS NULL").Scan(&emptyHistory))
	if !emptyHistory {
		t.Fatal("migration fixture was pre-populated from the latest template")
	}
}

func TestFixtureTemplateParallelCopies(t *testing.T) {
	for i := range 2 {
		t.Run(fmt.Sprintf("copy_%d", i), func(t *testing.T) {
			// Parallel subtests start after the parent returns. Their only shared resource
			// is the immutable source; identical table names must remain independent.
			t.Parallel()
			pool, _ := migratedTestSchema(t, "test_copy_parallel_")
			_, err := pool.Exec("CREATE TABLE fixture_parallel_marker(value integer NOT NULL)")
			must(t, err)
			_, err = pool.Exec("INSERT INTO fixture_parallel_marker VALUES ($1)", i)
			must(t, err)
			var count, value int
			must(t, pool.QueryRow("SELECT count(*),min(value) FROM fixture_parallel_marker").Scan(&count, &value))
			if count != 1 || value != i {
				t.Fatalf("parallel fixture rows = %d/%d, want 1/%d", count, value, i)
			}
		})
	}
}
