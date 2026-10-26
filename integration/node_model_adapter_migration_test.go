package integration

import "testing"

// An upgraded legacy schema gains only empty capability evidence; no existing Node is certified.
func TestMigration0060ModelAdapterFreshAndUpgrade(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		pool, _ := testSchema(t, "test_model_adapter_")
		if upgrade {
			applyMigrationsUpTo(t, pool, migrationsBefore(t, "0060_node_model_adapter.sql"))
		}
		migratedTwice(t, newStoreOnSchema(t, pool))
		var dataType, nullable, defaultValue string
		must(t, pool.QueryRow(`SELECT data_type,is_nullable,column_default FROM information_schema.columns
         WHERE table_schema=current_schema() AND table_name='node_instances' AND column_name='model_adapter'`).Scan(&dataType, &nullable, &defaultValue))
		if dataType != "jsonb" || nullable != "NO" || defaultValue != "'{}'::jsonb" {
			t.Fatalf("upgrade=%v: wrong legacy capability default %s/%s/%s", upgrade, dataType, nullable, defaultValue)
		}
	}
}
