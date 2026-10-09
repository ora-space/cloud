package core

import "fmt"

// The git identity an Agent run commits as (specs decisions/cloud/identity-access/
// 20260928-user-git-commit-identity.md, D1/D2).
//
// The wire contract requires it: an AgentSession carries `git_identity{name, email}`, and the Node
// injects it as GIT_AUTHOR_*/GIT_COMMITTER_* for the Agent and its children. Cloud therefore has to
// state one identity per AgentSession, and it states it in the session-start spec this package
// (`agent_run_session_start.go`) builds.
//
// What is implemented here is the ADR's *default* identity only: `{display name, {userId}@noreply}`
// derived from Cloud's own user row. The user-settable override the ADR's D1 describes (its
// `users.git_author_name` / `git_author_email` columns, the `/me/git-identity` API and the settings
// form) is not part of this change, and neither is a configurable noreply domain: the decision is
// still `proposed`, so Cloud states the one identity it can derive from authoritative state rather
// than inventing a stored profile field the decision has not yet approved.
//
// When the identity is resolved is D2's rule: at session start, in the transaction that builds the
// session-start spec, not at run creation. `runGitIdentity` honors an identity a run input already
// carries — the entry point for freezing at creation later — and otherwise resolves the trigger
// actor's identity then, so a display-name change between run creation and session start changes
// the identity that session commits as. Once the session-start spec is recorded it is the
// execution's fixed input and nothing later changes it.

// gitNoreplyDomain is the platform noreply domain the default identity's email is built on
// (identity-access D1's `git.noreply_domain`, proposed default). A `.invalid` TLD is deliberate: the
// address is a commit signature that must never be routable, and no deployment configures it yet.
const gitNoreplyDomain = "users.noreply.ora.invalid"

// defaultGitIdentity builds the identity D1 fixes for a user who has not stated one: the account's
// display name and a per-user noreply address. An account with no display name falls back to its own
// id rather than a platform-attributed name — an Agent's commit must never read as if the platform
// authored it on the user's behalf.
func defaultGitIdentity(t *transaction, uid string) (Object, error) {
	u := t.one("SELECT display_name FROM users WHERE id=$1", uid)
	if u == nil {
		return nil, fmt.Errorf("git identity: user %s is not a user row", uid)
	}
	name := u.S("displayName")
	if name == "" {
		name = uid
	}
	return Object{"name": name, "email": uid + "@" + gitNoreplyDomain}, nil
}

// runGitIdentity returns the identity an Agent run's session must commit as: the run input's frozen
// `gitIdentity` when it carries one, and otherwise the default identity of the user who triggered
// the run — the state every run in this repository is actually in. It never invents an identity: a
// run whose input names no identity and whose trigger actor cannot be resolved is an error the
// caller turns into a rolled-back transaction, because an AgentSession without a git identity is
// not dispatchable under the wire contract.
func runGitIdentity(t *transaction, run Object) (Object, error) {
	identity := run.O("input").O("gitIdentity")
	if identity.S("name") != "" && identity.S("email") != "" {
		return identity, nil
	}
	actor := runTriggerActor(t, run)
	return defaultGitIdentity(t, actor)
}
