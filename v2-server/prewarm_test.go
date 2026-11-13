//go:build linux

package main

import (
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
)

func TestNodeIdlePolicy(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	timeout, max, err := nodeIdlePolicy(env(nil))
	if err != nil || timeout != agentruntime.DefaultIdleTimeout || max != agentruntime.DefaultMaxIdle {
		t.Fatalf("defaults = %v %d %v", timeout, max, err)
	}
	timeout, max, err = nodeIdlePolicy(env(map[string]string{"V2_NODE_IDLE_TIMEOUT": "90m", "V2_NODE_IDLE_MAX": "3"}))
	if err != nil || timeout != 90*time.Minute || max != 3 {
		t.Fatalf("configured = %v %d %v", timeout, max, err)
	}
	for _, bad := range []map[string]string{{"V2_NODE_IDLE_TIMEOUT": "0s"}, {"V2_NODE_IDLE_TIMEOUT": "soon"}, {"V2_NODE_IDLE_MAX": "0"}, {"V2_NODE_IDLE_MAX": "-1"}} {
		if _, _, err := nodeIdlePolicy(env(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
