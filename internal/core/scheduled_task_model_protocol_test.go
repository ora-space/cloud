package core

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"
)

// TestScheduledTaskModelOverrideProtocolCompatibility pins the override gate the review asked for:
// a scheduled definition's selected model must be judged by the same declaration and runtime protocol
// checks as the declared Agent configuration, so a Claude agent backed by Anthropic cannot admit an
// OpenAI default-connection model into its binding.
func TestScheduledTaskModelOverrideProtocolCompatibility(t *testing.T) {
	scene := newPersonalAgentScene(t)
	scene.store.PersonalAgentRuntime = &PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
	if _, err := scene.request("PUT", "", newID(), scene.body(0)); err != nil {
		t.Fatal(err)
	}

	// Compatible: the selected model resolves on the Anthropic default and clears the Claude gate.
	_, err := scene.store.transact(context.Background(), func(tx *transaction) Object {
		_, connection, model := resolveDeclaredAgentRun(tx, scene.uid, scene.space, claudeAgentCLI, "1.0.0", "vendor/model-v1")
		if connection.S("protocol") != "anthropic-messages" || model.S("id") != "vendor/model-v1" {
			t.Fatalf("compatible override resolved to protocol=%q model=%q", connection.S("protocol"), model.S("id"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Incompatible: switch the personal default to an OpenAI connection. The same selected model now
	// resolves onto a connection Claude cannot execute, and must be refused by the protocol gate.
	openaiConn := modelRequest(t, scene.store, scene.claims, "POST", "/api/v1/me/model-connections", "", uuid.NewString(), modelBody("openai-completions")).O("resource")
	credentialID := uuid.NewString()
	ciphertext := make([]byte, 64)
	if _, err := rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	updated, err := scene.store.PutModelCredential(context.Background(), scene.claims, openaiConn.S("id"), openaiConn.N("version"), "", credentialID, ciphertext, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scene.store.Pool.Exec(`UPDATE personal_model_defaults SET connection_id=$1, model_id=$2, version=version+1 WHERE user_id=$3`, updated.S("id"), "vendor/model-v1", scene.uid); err != nil {
		t.Fatal(err)
	}
	_, err = scene.store.transact(context.Background(), func(tx *transaction) Object {
		_, _, _ = resolveDeclaredAgentRun(tx, scene.uid, scene.space, claudeAgentCLI, "1.0.0", "vendor/model-v1")
		return nil
	})
	expectModelFault(t, err, "agent_model_protocol_mismatch")
}
