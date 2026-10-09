package core

import "testing"

// TestSessionStartUsesTheTriggerUsersStatedGitIdentity pins identity-access D2: a session resolves
// the trigger user's identity when it starts — the stated one when there is one, the default
// otherwise — and an identity the run input already carries wins over both.
func TestSessionStartUsesTheTriggerUsersStatedGitIdentity(t *testing.T) {
	cases := []struct {
		name   string
		frozen Object
		stated Object
		want   func(actor, displayName string) Object
	}{
		{"default when none is stated", nil, nil, func(actor, displayName string) Object {
			return Object{"name": displayName, "email": actor + "@" + gitNoreplyDomain}
		}},
		{"the stated identity", nil, Object{"name": "Ada Lovelace", "email": "ada@example.invalid"}, func(string, string) Object {
			return Object{"name": "Ada Lovelace", "email": "ada@example.invalid"}
		}},
		{"a frozen run identity wins", Object{"name": "Frozen", "email": "frozen@example.invalid"}, Object{"name": "Ada", "email": "ada@example.invalid"}, func(string, string) Object {
			return Object{"name": "Frozen", "email": "frozen@example.invalid"}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := commandStore(t)
			input := sessionStartInput()
			delete(input, "gitIdentity")
			if c.frozen != nil {
				input["gitIdentity"] = c.frozen
			}
			scene := seedStartingRun(t, store, input)
			var actor, displayName string
			if err := store.Pool.QueryRow(`SELECT a.actor_id::text, u.display_name FROM issue_activities a JOIN users u ON u.id=a.actor_id
				WHERE a.action='run.enqueued' AND a.details->>'runId'=$1`, scene.seed.run).Scan(&actor, &displayName); err != nil {
				t.Fatalf("read trigger actor: %v", err)
			}
			if c.stated != nil {
				if _, err := store.Pool.Exec(`INSERT INTO user_git_identities(user_id,name,email) VALUES($1,$2,$3)`, actor, c.stated.S("name"), c.stated.S("email")); err != nil {
					t.Fatalf("state identity: %v", err)
				}
			}
			bindConnectedNode(t, store, scene.ws)
			runSessionStartPass(t, store)
			var raw []byte
			if err := store.Pool.QueryRow(`SELECT input FROM execution_work WHERE run_id=$1 AND kind='agent_session'`, scene.seed.run).Scan(&raw); err != nil {
				t.Fatalf("read session spec: %v", err)
			}
			got := mustObject(t, raw).O("gitIdentity")
			if want := c.want(actor, displayName); jsonText(got) != jsonText(want) {
				t.Fatalf("session git identity = %v, want %v", got, want)
			}
		})
	}
}

// TestGitIdentityValidation pins D1's shape rules, which the migration's CHECKs mirror.
func TestGitIdentityValidation(t *testing.T) {
	names := map[string]bool{"Ada": true, "张三": true, "": false, "a\nb": false, "Ada <x>": false}
	for name, ok := range names {
		if validGitName(name) != ok {
			t.Errorf("validGitName(%q) = %v, want %v", name, !ok, ok)
		}
	}
	emails := map[string]bool{"ada@example.invalid": true, "ada": false, "a b@x": false, "<a@x>": false, "a@b@c": false}
	for email, ok := range emails {
		if validGitEmail(email) != ok {
			t.Errorf("validGitEmail(%q) = %v, want %v", email, !ok, ok)
		}
	}
}
