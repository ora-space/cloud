package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// Real terminal takeover exposes only a finite safe code and keeps the Thread closed.
func TestSessionFailureProjectsOnlySafeCodes(t *testing.T) {
	for _, tc := range []struct{ detail, want string }{
		{"agent_turn_failed", "agent_turn_failed"},
		{"agent_authentication_required", "agent_authentication_required"},
		{"provider diagnostic must not be displayed", "agent_failed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedLiveThreadScene(t, f)
			scene.start(t, f)
			f.runningThread(t, scene)
			_, err := f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_AGENT_FAILED, tc.detail, "")
			must(t, err)
			page := f.getThread(scene.threadScene, "", 200)
			if page.S("threadState") != "ended" || page.S("failureCode") != tc.want || page.B("canAppend") {
				t.Fatalf("unsafe/incorrect failure projection: state=%s code=%s append=%t", page.S("threadState"), page.S("failureCode"), page.B("canAppend"))
			}
			rows := threadItems(t, page)
			last := rows[len(rows)-1]
			if last.S("source") != "system" || last.S("kind") != "session_failed" || last.O("record").S("code") != tc.want {
				t.Fatal("terminal failure was not appended as a bounded Cloud-owned Thread entry")
			}
			_, err = f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_AGENT_FAILED, tc.detail, "")
			must(t, err)
			if replay := threadItems(t, f.getThread(scene.threadScene, "", 200)); len(replay) != len(rows) {
				t.Fatal("terminal replay appended a duplicate failure entry")
			}
		})
	}
}
