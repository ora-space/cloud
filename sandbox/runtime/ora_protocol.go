package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	oraProtocolVersion = 1
	oraMaxMessageSize  = 16 * 1024 * 1024
	oraMaxFrameLength  = oraMaxMessageSize - 4
	oraStateDir        = "/workspace/.ora-node"
	oraRepoRoot        = "/workspace/repositories"
	oraWorktreeRoot    = "/workspace/worktrees"
)

type oraEnvelope struct {
	MessageType     string          `json:"message_type"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id,omitempty"`
	OperationID     string          `json:"operation_id,omitempty"`
	ExecutionID     string          `json:"execution_id,omitempty"`
	Sequence        uint64          `json:"sequence,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

type nodeIdentity struct {
	NodeID        string `json:"node_id"`
	IncarnationID string `json:"incarnation_id"`
}
type executionRecord struct {
	OperationID string         `json:"operation_id"`
	ExecutionID string         `json:"execution_id"`
	RequestID   string         `json:"request_id,omitempty"`
	State       string         `json:"state"`
	Action      string         `json:"action"`
	Spec        worktreeSpec   `json:"spec"`
	Sequence    uint64         `json:"sequence"`
	MessageType string         `json:"message_type"`
	Result      map[string]any `json:"result"`
	Acked       bool           `json:"acked"`
}
type durableState struct {
	NodeID       string                     `json:"node_id"`
	NextSequence uint64                     `json:"next_sequence"`
	Executions   map[string]executionRecord `json:"executions"`
}

var oraStore = struct {
	sync.Mutex
	state durableState
}{}

var (
	oraIncarnation = "inc-" + randomHex(12)
	oraUpgrader    = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func initOraState(expectedNodeID string) error {
	oraStore.Lock()
	defer oraStore.Unlock()
	if oraStore.state.NodeID != "" {
		if expectedNodeID != "" && oraStore.state.NodeID != expectedNodeID {
			return fmt.Errorf("persisted node identity does not match routed workspace")
		}
		return nil
	}
	if err := os.MkdirAll(oraStateDir, 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(oraStateDir, "state.json"))
	if err == nil {
		if err := json.Unmarshal(b, &oraStore.state); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if oraStore.state.NodeID == "" {
		oraStore.state.NodeID = expectedNodeID
		if oraStore.state.NodeID == "" {
			oraStore.state.NodeID = "node-" + randomHex(8)
		}
	}
	if oraStore.state.NextSequence == 0 {
		oraStore.state.NextSequence = 1
	}
	if oraStore.state.Executions == nil {
		oraStore.state.Executions = map[string]executionRecord{}
	}
	return saveOraStateLocked()
}

func saveOraStateLocked() error {
	b, err := json.MarshalIndent(oraStore.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(oraStateDir, "state.json.tmp")
	dst := filepath.Join(oraStateDir, "state.json")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func identity() nodeIdentity {
	oraStore.Lock()
	defer oraStore.Unlock()
	return nodeIdentity{oraStore.state.NodeID, oraIncarnation}
}

func oraNodeHandler(w http.ResponseWriter, r *http.Request) {
	if err := initOraState(strings.TrimSpace(r.Header.Get("X-Ora-Node-Id"))); err != nil {
		http.Error(w, "node state unavailable", 500)
		return
	}
	if token := os.Getenv("ORA_NODE_TOKEN"); token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := oraUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetReadLimit(oraMaxMessageSize)
	messageType, data, err := c.ReadMessage()
	if err != nil {
		return
	}
	if messageType != websocket.BinaryMessage {
		writeOraError(c, fmt.Errorf("ORA messages must be binary WebSocket messages"))
		return
	}
	msg, err := decodeOraFrame(data)
	if err != nil {
		writeOraError(c, err)
		return
	}
	if msg.MessageType != "hello" || msg.ProtocolVersion != oraProtocolVersion {
		writeOraError(c, fmt.Errorf("first message must be protocol v1 hello"))
		return
	}
	var hello struct {
		ControllerID      string `json:"controller_id"`
		SupportedVersions []int  `json:"supported_versions"`
	}
	if json.Unmarshal(msg.Payload, &hello) != nil || strings.TrimSpace(hello.ControllerID) == "" || !containsVersion(hello.SupportedVersions, 1) {
		writeOraError(c, fmt.Errorf("invalid hello"))
		return
	}
	expectedControllerID := strings.TrimSpace(r.Header.Get("X-Ora-Controller-Id"))
	if expectedControllerID == "" {
		expectedControllerID = strings.TrimSpace(os.Getenv("ORA_CONTROLLER_ID"))
	}
	if expectedControllerID != "" && hello.ControllerID != expectedControllerID {
		writeOraError(c, fmt.Errorf("controller identity is not authorized"))
		return
	}
	accepted := map[string]any{"message_type": "hello_accepted", "protocol_version": 1, "payload": map[string]any{"selected_version": 1, "node": identity(), "capabilities": []string{"worktree_execution"}}}
	if err = writeOraFrame(c, accepted); err != nil {
		return
	}
	if err = replayUnacked(c); err != nil {
		return
	}
	log.Printf("ora_node session_open controller_id=%q node_id=%q incarnation_id=%q", hello.ControllerID, identity().NodeID, oraIncarnation)
	for {
		messageType, data, err = c.ReadMessage()
		if err != nil {
			log.Printf("ora_node session_close controller_id=%q error=%v", hello.ControllerID, err)
			return
		}
		if messageType != websocket.BinaryMessage {
			writeOraError(c, fmt.Errorf("ORA messages must be binary WebSocket messages"))
			return
		}
		msg, err = decodeOraFrame(data)
		if err != nil {
			writeOraError(c, err)
			continue
		}
		if err = dispatchOra(c, &msg); err != nil {
			log.Printf("ora_node rejected type=%q operation_id=%q execution_id=%q error=%v", msg.MessageType, msg.OperationID, msg.ExecutionID, err)
			writeOraError(c, err)
		}
	}
}

func containsVersion(v []int, want int) bool {
	for _, n := range v {
		if n == want {
			return true
		}
	}
	return false
}

func decodeOraFrame(data []byte) (oraEnvelope, error) {
	var m oraEnvelope
	if len(data) < 5 {
		return m, fmt.Errorf("truncated frame")
	}
	n := int(binary.BigEndian.Uint32(data[:4]))
	if n < 1 || n > oraMaxFrameLength || n != len(data)-4 {
		return m, fmt.Errorf("invalid frame length")
	}
	if data[4] != 1 {
		return m, fmt.Errorf("unsupported frame type")
	}
	if err := json.Unmarshal(data[5:], &m); err != nil {
		return m, fmt.Errorf("invalid message json: %w", err)
	}
	if m.ProtocolVersion != 1 {
		return m, fmt.Errorf("unsupported protocol version")
	}
	return m, nil
}

func writeOraFrame(c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > oraMaxFrameLength-1 {
		return fmt.Errorf("encoded ORA frame exceeds 16 MiB")
	}
	frame := make([]byte, 5+len(b))
	// The protocol bound above makes this conversion safe.
	binary.BigEndian.PutUint32(frame[:4], uint32(len(b)+1)) // #nosec G115
	frame[4] = 1
	copy(frame[5:], b)
	return c.WriteMessage(websocket.BinaryMessage, frame)
}

func writeOraError(c *websocket.Conn, err error) {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()), time.Now().Add(time.Second))
}

func dispatchOra(c *websocket.Conn, m *oraEnvelope) error {
	switch m.MessageType {
	case "get_execution_status":
		return sendExecutionStatus(c, m)
	case "event_ack":
		return acknowledgeEvent(m)
	case "ensure_worktree", "remove_worktree":
		return executeWorktree(c, m)
	default:
		return fmt.Errorf("unsupported message_type %q", m.MessageType)
	}
}

func validateExecution(m *oraEnvelope) error {
	if strings.TrimSpace(m.OperationID) == "" || strings.TrimSpace(m.ExecutionID) == "" {
		return fmt.Errorf("operation_id and execution_id are required")
	}
	return nil
}

func sendExecutionStatus(c *websocket.Conn, m *oraEnvelope) error {
	if err := validateExecution(m); err != nil {
		return err
	}
	oraStore.Lock()
	rec, ok := oraStore.state.Executions[m.ExecutionID]
	oraStore.Unlock()
	state := map[string]any{"state": "unknown"}
	if ok {
		if rec.State == "completed" {
			state = map[string]any{"state": "completed", "result": map[string]any{"kind": resultKind(rec.MessageType), "result": rec.Result}}
		} else {
			state = map[string]any{"state": rec.State}
		}
	}
	return writeOraFrame(c, map[string]any{"message_type": "execution_status", "protocol_version": 1, "operation_id": m.OperationID, "execution_id": m.ExecutionID, "payload": map[string]any{"node": identity(), "state": state}})
}

func resultKind(t string) string {
	return map[string]string{"worktree_ready": "ready", "worktree_failed": "failed", "worktree_removed": "removed", "worktree_removal_failed": "removal_failed"}[t]
}

func acknowledgeEvent(m *oraEnvelope) error {
	if err := validateExecution(m); err != nil {
		return err
	}
	oraStore.Lock()
	defer oraStore.Unlock()
	rec, ok := oraStore.state.Executions[m.ExecutionID]
	if !ok {
		return nil
	}
	if m.Sequence != rec.Sequence {
		return fmt.Errorf("ack sequence mismatch")
	}
	rec.Acked = true
	oraStore.state.Executions[m.ExecutionID] = rec
	return saveOraStateLocked()
}

type worktreeSpec struct {
	NodeID        string `json:"node_id"`
	WorkspaceID   string `json:"workspace_id"`
	WorktreeID    string `json:"worktree_id"`
	Repository    string `json:"repository"`
	MainWorkspace struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
	} `json:"main_workspace"`
	BaseRef        string `json:"base_ref"`
	ExpectedBranch string `json:"expected_branch"`
	PathPolicy     struct {
		Kind          string `json:"kind"`
		DirectoryName string `json:"directory_name"`
	} `json:"path_policy"`
}
