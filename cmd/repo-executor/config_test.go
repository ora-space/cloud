package main

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8471" || c.Slots != 4 || c.MaxTaskBytes != 2<<30 || c.SyncTimeoutMax != 30*time.Minute ||
		c.PublishTimeoutMax != 5*time.Minute || c.ProbeTimeoutMax != 5*time.Minute || c.GitServerURL != "https://ora-git-service:8447" ||
		c.CredentialStoreURL != "https://credential-store:8461" || c.WorkRoot != "/var/lib/repo-executor/tasks" || c.HelperPath != "/usr/local/bin/git-credential-ora" {
		t.Fatalf("defaults = %+v", c)
	}
	// The executor's own peers are always internal names.
	if got := c.internalHosts(); !reflect.DeepEqual(got, []string{"ora-git-service", "credential-store"}) {
		t.Fatalf("internal hosts = %v", got)
	}
}

func TestLoadConfigOverridesAndRefusals(t *testing.T) {
	env := map[string]string{
		"REPO_EXECUTOR_SLOTS": "2", "REPO_EXECUTOR_EGRESS_ALLOW": "127.0.0.1:9443, fixture.test ", "REPO_EXECUTOR_EGRESS_DENY_HOSTS": "corp.internal",
		"REPO_EXECUTOR_PUBLISH_TIMEOUT_MAX": "2m",
	}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Slots != 2 || !reflect.DeepEqual(c.EgressAllow, []string{"127.0.0.1:9443", "fixture.test"}) || c.ProbeTimeoutMax != 2*time.Minute {
		t.Fatalf("config = %+v", c)
	}
	for key, value := range map[string]string{
		"REPO_EXECUTOR_SLOTS": "0", "REPO_EXECUTOR_MAX_TASK_BYTES": "-1", "REPO_EXECUTOR_SYNC_TIMEOUT_MAX": "soon",
		"REPO_EXECUTOR_ADDR": "8471", "REPO_EXECUTOR_GIT_SERVER_URL": "http://git", "REPO_EXECUTOR_WORK_ROOT": "relative",
	} {
		if _, err := loadConfig(func(k string) string {
			if k == key {
				return value
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s was accepted", key, value)
		}
	}
}
