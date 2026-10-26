package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The new task queue must leave every existing run untouched while constraining new messages.
func TestMigration0046TaskConversationFreshAndUpgrade(t *testing.T) {
	fresh, _ := testSchema(t, "test_tconv45f_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	for _, table := range []string{"task_conversations", "task_conversation_messages", "task_conversation_scan_state"} {
		if !tableExists(t, fresh, table) {
			t.Fatalf("fresh schema missing %s", table)
		}
	}
	var scanRows int
	must(t, fresh.QueryRow("SELECT count(*) FROM task_conversation_scan_state WHERE singleton=true").Scan(&scanRows))
	if scanRows != 1 {
		t.Fatalf("fresh queue scan has %d cursor rows", scanRows)
	}
	var projected bool
	must(t, fresh.QueryRow(`SELECT count(*)=2 FROM information_schema.columns WHERE table_schema=current_schema()
		AND table_name='thread_entries' AND column_name IN ('visible_text','visible_text_truncated')`).Scan(&projected))
	if !projected {
		t.Fatal("fresh Thread schema is missing Node's optional text projection")
	}

	pool, _ := testSchema(t, "test_tconv45u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0046_task_agent_conversations.sql"))
	seed := seedOwnershipChain(t, pool)
	var before, after string
	must(t, pool.QueryRow("SELECT row_to_json(r)::text FROM issue_runs r WHERE id=$1", seed.runID).Scan(&before))
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))
	must(t, pool.QueryRow("SELECT row_to_json(r)::text FROM issue_runs r WHERE id=$1", seed.runID).Scan(&after))
	if before != after {
		t.Fatal("task conversation migration rewrote an existing run")
	}
	var count int
	must(t, pool.QueryRow("SELECT count(*) FROM task_conversation_messages").Scan(&count))
	if count != 0 {
		t.Fatal("legacy records were given invented task message identities")
	}
	must(t, pool.QueryRow("SELECT count(*) FROM task_conversations").Scan(&count))
	if count != 0 {
		t.Fatal("legacy issues were backfilled with invented conversation state")
	}
	must(t, pool.QueryRow("SELECT 1 FROM issues WHERE id=$1", seed.issueID).Scan(&count))
	_, err := pool.Exec("INSERT INTO task_conversations(issue_id,tenant_id,last_seq) VALUES($1,$2,1)", seed.issueID, seed.tenantID)
	must(t, err)
	insert := func(id, body string) error {
		_, err := pool.Exec(`INSERT INTO task_conversation_messages(id,tenant_id,issue_id,seq,user_id,agent_id,body,state) VALUES($1,$2,$3,1,$4,$5,$6,'waiting')`, id, seed.tenantID, seed.issueID, seed.userID, uuid.NewString(), body)
		return err
	}
	must(t, insert(uuid.NewString(), "new user instruction"))
	wantPGError(t, insert(uuid.NewString(), "duplicate position"), "23505")
	wantPGError(t, insert(uuid.NewString(), strings.Repeat("x", 65537)), "23514")
}

func TestMigration0048DoesNotInventTaskPauses(t *testing.T) {
	pool, _ := testSchema(t, "test_pause47_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0048_task_conversation_manual_pause.sql"))
	seed := seedOwnershipChain(t, pool)
	_, err := pool.Exec("INSERT INTO task_conversations(issue_id,tenant_id,last_seq) VALUES($1,$2,0)", seed.issueID, seed.tenantID)
	must(t, err)
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	var paused *string
	must(t, pool.QueryRow("SELECT paused_run_id FROM task_conversations WHERE issue_id=$1", seed.issueID).Scan(&paused))
	if paused != nil {
		t.Fatal("migration paused an existing task without a user stop")
	}
}
