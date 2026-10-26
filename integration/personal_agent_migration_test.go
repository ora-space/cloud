package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestMigration0050PersonalAgentConfigurationsUpgrade(t *testing.T) {
	pool, _ := testSchema(t, "test_agent_configuration_upgrade_")
	entries, err := os.ReadDir(filepath.Join("..", "internal", "core", "migrations"))
	must(t, err)
	var versions []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".sql" && entry.Name() < "0050" {
			versions = append(versions, entry.Name())
		}
	}
	applyMigrationsUpTo(t, pool, versions)
	user, connection := uuid.NewString(), uuid.NewString()
	_, err = pool.Exec("INSERT INTO users(id,display_name,status) VALUES($1,'Historical','active')", user)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO personal_model_connections(id,user_id,name,protocol,base_url,auth_mode,models)
		VALUES($1,$2,'Historical','anthropic-messages','https://models.example.invalid/v1','bearer','[{"id":"historical"}]')`, connection, user)
	must(t, err)
	_, err = pool.Exec("INSERT INTO personal_model_defaults(user_id,connection_id,model_id,version) VALUES($1,$2,'historical',7)", user, connection)
	must(t, err)
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(t.Context()))
	must(t, store.Migrate(t.Context()))
	must(t, store.CheckSchema(t.Context()))
	var actual string
	var version, configurations, snapshots int
	must(t, pool.QueryRow("SELECT connection_id,version FROM personal_model_defaults WHERE user_id=$1", user).Scan(&actual, &version))
	must(t, pool.QueryRow("SELECT (SELECT count(*) FROM personal_agent_configurations),(SELECT count(*) FROM run_agent_configurations)").Scan(&configurations, &snapshots))
	if actual != connection || version != 7 || configurations != 0 || snapshots != 0 {
		t.Fatalf("migration changed history: %s %d %d %d", actual, version, configurations, snapshots)
	}
}
