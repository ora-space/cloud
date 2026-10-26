package core

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// The frozen prompt budget is measured after JSON escaping and aggregating Node projections.
func TestTaskConversationRetainsOversizeRecentTurn(t *testing.T) {
	for name, blocks := range map[string][]string{
		"single":  {strings.Repeat("a", 65536)},
		"blocks":  {strings.Repeat("界", 10000), strings.Repeat("好", 14000)},
		"escaped": {strings.Repeat("\"\\\n", 20000)},
	} {
		t.Run(name, func(t *testing.T) {
			store := dispatcherDB(t)
			seed := seedDispatchScene(t, store.Pool, false)
			var snapshot Object
			_, err := store.transact(context.Background(), func(tx *transaction) Object {
				turn := newID()
				tx.exec(`UPDATE issue_runs SET status='completed',phase='done',input='{"task":"recent question"}' WHERE id=$1`, seed.run)
				tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{}',$2)`, seed.run, turn)
				for index, block := range blocks {
					tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text) VALUES($1,$2,'node','update','{}',$3,$4)`, seed.run, index+2, turn, block)
				}
				snapshot = buildRunContext(tx, issue(tx, seed.tenant, seed.issue), "next question", "agent", seed.agent, Object{}, nil)
				return Object{}
			})
			if err != nil {
				t.Fatal(err)
			}
			history, ok := snapshot["conversationHistory"].([]Object)
			if !ok || len(history) != 1 {
				t.Fatalf("latest oversize turn disappeared: %v", snapshot["conversationHistory"])
			}
			assistant := history[0].S("assistant")
			if assistant == "" || !utf8.ValidString(assistant) || !strings.HasPrefix(strings.Join(blocks, ""), assistant) || !history[0].B("assistantTruncated") || history[0].S("user") != "recent question" || !snapshot.B("conversationHistoryIncomplete") {
				t.Fatal("truncation lost the recent user input, UTF-8 prefix, or incompleteness evidence")
			}
			if len(renderAgentInitialTurn(snapshot, nil)) > taskConversationPromptBudget-1024 {
				t.Fatal("escaped frozen history exceeded the prompt budget")
			}
		})
	}
}
