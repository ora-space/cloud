//go:build linux

package main

import (
	"errors"
	"fmt"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"testing"
)

func TestSkillJobOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want store.SkillJobOutcome
	}{
		{"success", nil, store.SkillJobSucceeded},
		{"download retry", fmt.Errorf("download: %w", pluginmarket.ErrTransient), store.SkillJobRetry},
		{"node exit", fmt.Errorf("node: %w", execution.ErrUnavailable), store.SkillJobRetry},
		{"archive rejected", pluginmarket.ErrPackage, store.SkillJobFailed},
		{"unexpected", errors.New("unexpected"), store.SkillJobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillJobOutcome(tc.err); got != tc.want {
				t.Fatalf("skillJobOutcome(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
