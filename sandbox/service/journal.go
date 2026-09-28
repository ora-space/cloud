package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *service) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.statePath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cfg.statePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, s.cfg.statePath); err != nil {
		return err
	}
	// Persist the directory entry as well as the file contents so an accepted
	// intent survives a host power loss after rename returns.
	dir, err := os.Open(filepath.Dir(s.cfg.statePath))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func cloneEffect(entry *effectEntry) *effectEntry {
	if entry == nil {
		return nil
	}
	cloned := *entry
	if entry.Result != nil {
		cloned.Result = map[string]any{}
		for k, v := range entry.Result {
			cloned.Result[k] = v
		}
	}
	return &cloned
}

func cloneWorkspace(ws *workspaceState) *workspaceState {
	if ws == nil {
		return nil
	}
	cloned := *ws
	cloned.Tombstones = map[string]bool{}
	for k, v := range ws.Tombstones {
		cloned.Tombstones[k] = v
	}
	return &cloned
}

func workspaceTag(workspaceID, effectID string) string {
	return "ws-" + workspaceID + "-" + strings.ReplaceAll(effectID, "-", "")[:8]
}

func nodeID(workspaceID string) string { return "workspace-" + workspaceID }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
