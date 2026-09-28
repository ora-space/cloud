package main

import (
	"net/http"
	"time"
)

func validateRequest(r effectRequest) string {
	if !uuidPattern.MatchString(r.ProjectID) || !uuidPattern.MatchString(r.WorkspaceID) {
		return "invalid_scope"
	}
	switch r.Kind {
	case "sandbox_ensure", "workspace_data_delete":
		if r.SandboxInstanceID != "" {
			return "invalid_request"
		}
	case "sandbox_terminate":
		if !uuidPattern.MatchString(r.SandboxInstanceID) {
			return "invalid_request"
		}
	default:
		return "unsupported_kind"
	}
	return ""
}

func (s *service) putEffect(id string, request effectRequest) (effectResult *effectEntry, status int, code string) {
	s.ops.Lock()
	defer s.ops.Unlock()

	s.mu.Lock()
	existing, effectExists := s.db.Effects[id]
	if effectExists {
		if existing.Request != request {
			s.mu.Unlock()
			return nil, http.StatusConflict, "effect_conflict"
		}
		if existing.State == "succeeded" {
			effectSnapshot := cloneEffect(existing)
			s.mu.Unlock()
			return effectSnapshot, http.StatusOK, ""
		}
	}
	owner := s.workspaceOwnerLocked(request.WorkspaceID)
	if owner != "" && owner != request.ProjectID {
		s.mu.Unlock()
		return nil, http.StatusConflict, "scope_conflict"
	}
	if request.Kind == "sandbox_terminate" {
		target := s.db.Effects[request.SandboxInstanceID]
		if target == nil {
			s.mu.Unlock()
			return nil, http.StatusNotFound, "sandbox_not_found"
		}
		if target.Request.Kind != "sandbox_ensure" || target.Request.ProjectID != request.ProjectID || target.Request.WorkspaceID != request.WorkspaceID {
			s.mu.Unlock()
			return nil, http.StatusConflict, "scope_conflict"
		}
	}
	ws, ok := s.db.Workspaces[request.WorkspaceID]
	if !ok {
		ws = &workspaceState{ProjectID: request.ProjectID, WorkspaceID: request.WorkspaceID, Tombstones: map[string]bool{}}
		s.db.Workspaces[request.WorkspaceID] = ws
	}
	if ws.Tombstones == nil {
		ws.Tombstones = map[string]bool{}
	}
	entry := existing
	if !effectExists {
		entry = &effectEntry{ID: id, ExternalID: id, Request: request}
		s.db.Effects[id] = entry
	}
	entry.State, entry.Error, entry.Result = "running", "", nil
	switch request.Kind {
	case "sandbox_ensure":
		switch {
		case ws.ActiveSandboxID != "" && ws.ActiveSandboxID != id:
			entry.State, entry.Error = "failed", "workspace_busy"
		case ws.Tombstones[id]:
			entry.State, entry.Error = "failed", "sandbox_terminated"
		default:
			// Reassert the reservation on every failed/running retry. An ensure
			// that first failed with workspace_busy may become eligible later.
			ws.ActiveSandboxID = id
		}
	case "sandbox_terminate":
		ws.Tombstones[request.SandboxInstanceID] = true
		if ws.PendingTag == "" {
			ws.PendingTag = workspaceTag(request.WorkspaceID, id)
		}
	case "workspace_data_delete":
		if ws.ActiveSandboxID != "" {
			entry.State, entry.Error = "failed", "workspace_active"
		}
	}
	if err := s.saveLocked(); err != nil {
		if !effectExists {
			delete(s.db.Effects, id)
		}
		s.mu.Unlock()
		return nil, http.StatusServiceUnavailable, "journal_unavailable"
	}
	if entry.State == "failed" {
		effectSnapshot := cloneEffect(entry)
		s.mu.Unlock()
		return effectSnapshot, http.StatusServiceUnavailable, ""
	}
	s.mu.Unlock()

	var result map[string]any
	var effectError string
	switch request.Kind {
	case "sandbox_ensure":
		result, effectError = s.ensureSandbox(id, request)
	case "sandbox_terminate":
		result, effectError = s.terminateSandbox(id, request)
	case "workspace_data_delete":
		result, effectError = s.deleteWorkspaceData(request)
	}
	s.mu.Lock()
	entry = s.db.Effects[id]
	if effectError == "" {
		entry.State = "succeeded"
		entry.Result = result
		entry.Error = ""
	} else {
		entry.State = "failed"
		entry.Result = nil
		entry.Error = effectError
	}
	if err := s.saveLocked(); err != nil {
		entry.State = "failed"
		entry.Result = nil
		entry.Error = "journal_unavailable"
	}
	effectSnapshot := cloneEffect(entry)
	s.mu.Unlock()
	if effectSnapshot.State == "failed" {
		return effectSnapshot, http.StatusServiceUnavailable, ""
	}
	return effectSnapshot, http.StatusOK, ""
}

func (s *service) workspaceOwnerLocked(workspaceID string) string {
	if ws := s.db.Workspaces[workspaceID]; ws != nil {
		return ws.ProjectID
	}
	for _, entry := range s.db.Effects {
		if entry.Request.WorkspaceID == workspaceID {
			return entry.Request.ProjectID
		}
	}
	return ""
}

func (s *service) ensureSandbox(id string, request effectRequest) (result map[string]any, errorCode string) {
	s.mu.Lock()
	ws := cloneWorkspace(s.db.Workspaces[request.WorkspaceID])
	s.mu.Unlock()
	if ws == nil || ws.Tombstones[id] {
		return nil, "sandbox_terminated"
	}
	exists, _, err := s.actorState(id)
	if err != nil {
		return nil, "platform_unavailable"
	}
	if !exists {
		args := []string{"create", "actor", id, "-a", s.cfg.atespace}
		if ws.CurrentTag != "" {
			// kubectl-ate currently validates --template even when --tag supplies
			// the snapshot. Passing both also pins the runtime template used for
			// the restored data snapshot.
			args = append(args, "--tag", ws.CurrentTag, "--template", s.cfg.template)
		} else {
			args = append(args, "--template", s.cfg.template)
		}
		if _, err = s.runAte(60*time.Second, args...); err != nil {
			return nil, "platform_unavailable"
		}
	}
	if err = s.waitReady(id, 45*time.Second); err != nil {
		return nil, "node_unreachable"
	}
	return map[string]any{"sandboxInstanceId": id, "nodeId": nodeID(request.WorkspaceID)}, ""
}

func (s *service) terminateSandbox(effectID string, request effectRequest) (result map[string]any, errorCode string) {
	target := request.SandboxInstanceID
	s.mu.Lock()
	ws := cloneWorkspace(s.db.Workspaces[request.WorkspaceID])
	ensure := cloneEffect(s.db.Effects[target])
	s.mu.Unlock()
	if ws == nil {
		return map[string]any{"terminated": true}, ""
	}
	exists, state, err := s.actorState(target)
	if err != nil {
		return nil, "platform_unavailable"
	}
	if !exists {
		if ensure == nil || ensure.State != "succeeded" {
			s.finalizeTermination(request.WorkspaceID, target, "")
			return map[string]any{"terminated": true}, ""
		}
		if ws.PendingTag != "" {
			ready, tagErr := s.tagReady(ws.PendingTag)
			if tagErr != nil {
				return nil, "platform_unavailable"
			}
			if ready {
				s.finalizeTermination(request.WorkspaceID, target, ws.PendingTag)
				return map[string]any{"terminated": true}, ""
			}
		}
		return nil, "result_unknown"
	}
	if state != "ACTOR_STATE_SUSPENDED" {
		if _, err = s.runAte(90*time.Second, "suspend", "actor", target, "-a", s.cfg.atespace); err != nil {
			return nil, "platform_unavailable"
		}
	}
	if err = s.waitActorState(target, "ACTOR_STATE_SUSPENDED", 45*time.Second); err != nil {
		return nil, "platform_unavailable"
	}
	tag := ws.PendingTag
	if tag == "" {
		tag = workspaceTag(request.WorkspaceID, effectID)
		s.mu.Lock()
		s.db.Workspaces[request.WorkspaceID].PendingTag = tag
		_ = s.saveLocked()
		s.mu.Unlock()
	}
	tagExists, ready, err := s.tagStatus(tag)
	if err != nil {
		return nil, "platform_unavailable"
	}
	if !tagExists {
		if _, err = s.runAte(90*time.Second, "create", "tag", tag, "--actor", target, "-a", s.cfg.atespace); err != nil {
			return nil, "platform_unavailable"
		}
	}
	if !ready {
		if err = s.waitTagReady(tag, 60*time.Second); err != nil {
			return nil, "platform_unavailable"
		}
	}
	if _, err = s.runAte(60*time.Second, "delete", "actor", target, "-a", s.cfg.atespace, "--any-state"); err != nil {
		return nil, "platform_unavailable"
	}
	if err = s.waitActorAbsent(target, 30*time.Second); err != nil {
		return nil, "platform_unavailable"
	}
	oldTag := ws.CurrentTag
	s.finalizeTermination(request.WorkspaceID, target, tag)
	if oldTag != "" && oldTag != tag {
		_, _ = s.runAte(30*time.Second, "delete", "tag", oldTag, "-a", s.cfg.atespace)
	}
	return map[string]any{"terminated": true}, ""
}

func (s *service) finalizeTermination(workspaceID, target, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.db.Workspaces[workspaceID]
	if ws == nil {
		return
	}
	if ws.ActiveSandboxID == target {
		ws.ActiveSandboxID = ""
	}
	if tag != "" {
		ws.CurrentTag = tag
	}
	ws.PendingTag = ""
	_ = s.saveLocked()
}

func (s *service) deleteWorkspaceData(request effectRequest) (result map[string]any, errorCode string) {
	s.mu.Lock()
	ws := cloneWorkspace(s.db.Workspaces[request.WorkspaceID])
	s.mu.Unlock()
	if ws == nil {
		return map[string]any{"removed": true}, ""
	}
	if ws.ActiveSandboxID != "" {
		return nil, "workspace_active"
	}
	for _, tag := range []string{ws.PendingTag, ws.CurrentTag} {
		if tag == "" {
			continue
		}
		exists, _, err := s.tagStatus(tag)
		if err != nil {
			return nil, "platform_unavailable"
		}
		if exists {
			if _, err = s.runAte(45*time.Second, "delete", "tag", tag, "-a", s.cfg.atespace); err != nil {
				return nil, "platform_unavailable"
			}
		}
	}
	s.mu.Lock()
	delete(s.db.Workspaces, request.WorkspaceID)
	_ = s.saveLocked()
	s.mu.Unlock()
	return map[string]any{"removed": true}, ""
}

func (s *service) reconcile(id string) {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.mu.Lock()
	entry := cloneEffect(s.db.Effects[id])
	s.mu.Unlock()
	if entry == nil || entry.State != "running" {
		return
	}
	var result map[string]any
	resolved := false
	switch entry.Request.Kind {
	case "sandbox_ensure":
		exists, state, err := s.actorState(id)
		if err == nil && exists && state == "ACTOR_STATE_RUNNING" {
			result = map[string]any{"sandboxInstanceId": id, "nodeId": nodeID(entry.Request.WorkspaceID)}
			resolved = true
		}
	case "sandbox_terminate":
		exists, _, err := s.actorState(entry.Request.SandboxInstanceID)
		if err == nil && !exists {
			s.mu.Lock()
			ws := cloneWorkspace(s.db.Workspaces[entry.Request.WorkspaceID])
			s.mu.Unlock()
			if ws != nil && ws.PendingTag != "" {
				ready, _ := s.tagReady(ws.PendingTag)
				if ready {
					s.finalizeTermination(entry.Request.WorkspaceID, entry.Request.SandboxInstanceID, ws.PendingTag)
					result, resolved = map[string]any{"terminated": true}, true
				}
			} else {
				result, resolved = map[string]any{"terminated": true}, true
			}
		}
	case "workspace_data_delete":
		s.mu.Lock()
		_, exists := s.db.Workspaces[entry.Request.WorkspaceID]
		s.mu.Unlock()
		if !exists {
			result, resolved = map[string]any{"removed": true}, true
		}
	}
	if resolved {
		s.mu.Lock()
		current := s.db.Effects[id]
		if current != nil && current.State == "running" {
			current.State, current.Result, current.Error = "succeeded", result, ""
			_ = s.saveLocked()
		}
		s.mu.Unlock()
	}
}
