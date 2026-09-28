package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func (s *service) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusUpgradeRequired, "websocket_upgrade_required")
		return
	}
	target := r.Header.Get("Ate-Target-Actor")
	parts := strings.Split(target, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] != s.cfg.atespace || strings.Contains(parts[1], "/") {
		writeError(w, http.StatusBadRequest, "invalid_actor_target")
		return
	}
	sandboxID := parts[1]
	nodeID, routeStatus := s.routeIdentity(sandboxID)
	if routeStatus != 0 {
		writeError(w, routeStatus, http.StatusText(routeStatus))
		return
	}
	exists, state, err := s.actorState(sandboxID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "actor_discovery_failed")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "sandbox_not_found")
		return
	}
	if state != "ACTOR_STATE_RUNNING" {
		writeError(w, http.StatusServiceUnavailable, "sandbox_unreachable")
		return
	}
	base, _ := url.Parse(s.cfg.internalRouter)
	base.Path, base.RawQuery = r.URL.Path, r.URL.RawQuery
	headers := http.Header{}
	headers.Set("Ate-Target-Actor", target)
	headers.Set("X-Ora-Node-Id", nodeID)
	headers.Set("X-Ora-Sandbox-Instance", sandboxID)
	headers.Set("X-Ora-Controller-Id", s.cfg.controllerID)
	subprotocols := websocket.Subprotocols(r)
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Subprotocols: subprotocols}
	headers.Set("Host", sandboxID+"."+s.cfg.atespace+".actors.resources.substrate.ate.dev")
	upstream, response, err := dialer.Dial(base.String(), headers)
	if err != nil {
		status := http.StatusBadGateway
		if response != nil {
			if response.StatusCode == 404 || response.StatusCode == 503 || response.StatusCode == 504 {
				status = response.StatusCode
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		writeError(w, status, "node_connection_failed")
		return
	}
	defer upstream.Close()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	if selected := upstream.Subprotocol(); selected != "" {
		upgrader.Subprotocols = []string{selected}
	}
	downstream, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer downstream.Close()
	upstream.SetReadLimit(maxWebSocketMessage)
	downstream.SetReadLimit(maxWebSocketMessage)
	proxyWebSockets(downstream, upstream)
}

func (s *service) routeIdentity(sandboxID string) (routedNodeID string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.db.Effects[sandboxID]
	if entry == nil || entry.Request.Kind != "sandbox_ensure" {
		return "", http.StatusNotFound
	}
	ws := s.db.Workspaces[entry.Request.WorkspaceID]
	if ws == nil || ws.Tombstones[sandboxID] || ws.ActiveSandboxID != sandboxID {
		return "", http.StatusNotFound
	}
	if entry.State != "succeeded" {
		return "", http.StatusServiceUnavailable
	}
	return nodeID(entry.Request.WorkspaceID), 0
}

func proxyWebSockets(a, b *websocket.Conn) {
	var once sync.Once
	done := make(chan struct{})
	copyDirection := func(dst, src *websocket.Conn) {
		for {
			typ, data, err := src.ReadMessage()
			if err != nil {
				code, reason := websocket.CloseInternalServerErr, "peer connection lost"
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					code, reason = closeErr.Code, closeErr.Text
				}
				_ = dst.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
				once.Do(func() { close(done) })
				return
			}
			if err = dst.WriteMessage(typ, data); err != nil {
				once.Do(func() { close(done) })
				return
			}
		}
	}
	go copyDirection(b, a)
	go copyDirection(a, b)
	<-done
}
