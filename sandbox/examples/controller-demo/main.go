package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

const maxWebSocketMessage = 16 * 1024 * 1024

func frame(v any) []byte {
	b, _ := json.Marshal(v)
	if len(b) > maxWebSocketMessage-5 {
		panic("ORA frame exceeds 16 MiB")
	}
	out := make([]byte, len(b)+5)
	// The 16 MiB bound above makes this conversion safe on every supported target.
	binary.BigEndian.PutUint32(out[:4], uint32(len(b)+1)) // #nosec G115
	out[4] = 1
	copy(out[5:], b)
	return out
}

func helloNodeID(accepted map[string]any) string {
	payload, ok := accepted["payload"].(map[string]any)
	if !ok {
		panic("hello_accepted payload is missing")
	}
	node, ok := payload["node"].(map[string]any)
	if !ok {
		panic("hello_accepted node is missing")
	}
	nodeID, ok := node["node_id"].(string)
	if !ok || nodeID == "" {
		panic("hello_accepted node_id is missing")
	}
	return nodeID
}

func receive(c *websocket.Conn) map[string]any {
	_, b, err := c.ReadMessage()
	if err != nil {
		panic(err)
	}
	if len(b) < 5 || b[4] != 1 || int(binary.BigEndian.Uint32(b[:4])) != len(b)-4 {
		panic("invalid ORA frame")
	}
	var v map[string]any
	if err = json.Unmarshal(b[5:], &v); err != nil {
		panic(err)
	}
	line, _ := json.Marshal(v)
	fmt.Println(string(line))
	return v
}

func send(c *websocket.Conn, v any) {
	if err := c.WriteMessage(websocket.BinaryMessage, frame(v)); err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) != 3 {
		panic("usage: ora-controller-demo ACTOR ACTION")
	}
	actor, action := os.Args[1], os.Args[2]
	h := http.Header{"Ate-Target-Actor": []string{"ate-coding-poc/" + actor}}
	c, response, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:18001/ora-node/v1", h)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		panic(err)
	}
	defer c.Close()
	send(c, map[string]any{"message_type": "hello", "protocol_version": 1, "payload": map[string]any{"controller_id": "ora-cloud-controller", "supported_versions": []int{1}}})
	accepted := receive(c)
	node := helloNodeID(accepted)
	if action == "replay" {
		receive(c)
		return
	}
	spec := map[string]any{"node_id": node, "workspace_id": "workspace-task", "worktree_id": "worktree-demo", "repository": "demo", "main_workspace": map[string]any{"workspace_id": "workspace-main", "path": "/workspace/repositories/demo"}, "base_ref": "refs/heads/main", "expected_branch": "ora/demo", "path_policy": map[string]any{"kind": "node_managed", "directory_name": "workspace-task"}}
	var msg map[string]any
	switch action {
	case "ensure", "ensure-noack":
		msg = map[string]any{"message_type": "ensure_worktree", "protocol_version": 1, "request_id": "request-ensure", "operation_id": "operation-ensure", "execution_id": "execution-ensure", "payload": map[string]any{"spec": spec}}
	case "status":
		msg = map[string]any{"message_type": "get_execution_status", "protocol_version": 1, "operation_id": "operation-ensure", "execution_id": "execution-ensure", "payload": map[string]any{"node_id": node}}
	case "duplicate":
		msg = map[string]any{"message_type": "ensure_worktree", "protocol_version": 1, "request_id": "request-ensure", "operation_id": "operation-ensure", "execution_id": "execution-ensure", "payload": map[string]any{"spec": spec}}
	case "remove":
		msg = map[string]any{"message_type": "remove_worktree", "protocol_version": 1, "request_id": "request-remove", "operation_id": "operation-remove", "execution_id": "execution-remove", "payload": map[string]any{"spec": spec}}
	default:
		panic("unknown action")
	}
	send(c, msg)
	result := receive(c)
	if seq, ok := result["sequence"]; ok && action != "ensure-noack" {
		send(c, map[string]any{"message_type": "event_ack", "protocol_version": 1, "operation_id": result["operation_id"], "execution_id": result["execution_id"], "sequence": seq, "payload": map[string]any{"node_id": node}})
	}
}
