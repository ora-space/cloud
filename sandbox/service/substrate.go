package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

func (s *service) actorState(name string) (exists bool, state string, resultErr error) {
	out, err := s.runAte(20*time.Second, "get", "actor", name, "-a", s.cfg.atespace, "-o", "json")
	if err != nil {
		if strings.Contains(strings.ToLower(out), "not found") || strings.Contains(strings.ToLower(out), "no actors") {
			return false, "", nil
		}
		return false, "", err
	}
	var response struct {
		Actors []struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"actors"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return false, "", err
	}
	if len(response.Actors) == 0 {
		return false, "", nil
	}
	return true, response.Actors[0].Status.State, nil
}

func (s *service) waitReady(actor string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u, _ := url.Parse(strings.Replace(s.cfg.internalRouter, "ws://", "http://", 1) + "/readyz")
		req, _ := http.NewRequest(http.MethodGet, u.String(), http.NoBody)
		req.Host = actor + "." + s.cfg.atespace + ".actors.resources.substrate.ate.dev"
		req.Header.Set("Ate-Target-Actor", s.cfg.atespace+"/"+actor)
		resp, err := s.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("actor readiness timeout")
}

func (s *service) waitActorState(actor, wanted string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exists, state, err := s.actorState(actor)
		if err == nil && exists && state == wanted {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("actor state timeout")
}

func (s *service) waitActorAbsent(actor string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exists, _, err := s.actorState(actor)
		if err == nil && !exists {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("actor deletion timeout")
}

func (s *service) tagReady(tag string) (bool, error) {
	_, ready, err := s.tagStatus(tag)
	return ready, err
}

func (s *service) tagStatus(tag string) (exists, ready bool, resultErr error) {
	out, err := s.runAte(20*time.Second, "get", "tag", tag, "-a", s.cfg.atespace, "-o", "json")
	if err != nil {
		if strings.Contains(strings.ToLower(out), "not found") || strings.Contains(strings.ToLower(out), "no tags") {
			return false, false, nil
		}
		return false, false, err
	}
	var response struct {
		Tags []struct {
			Status struct {
				Snapshot *struct {
					SnapshotURI string `json:"snapshotUri"`
				} `json:"snapshot"`
			} `json:"status"`
		} `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return false, false, err
	}
	if len(response.Tags) == 0 {
		return false, false, nil
	}
	ready = response.Tags[0].Status.Snapshot != nil && response.Tags[0].Status.Snapshot.SnapshotURI != ""
	return true, ready, nil
}

func (s *service) waitTagReady(tag string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ready, err := s.tagReady(tag)
		if err == nil && ready {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("tag readiness timeout")
}

func (s *service) runAte(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"--kubeconfig", s.cfg.kubeconfig, "--context", s.cfg.context}, args...)
	// The executable path is operator configuration and every argument is built
	// by this service; no shell or caller-provided command text is involved.
	cmd := exec.CommandContext(ctx, s.cfg.kubectlAte, full...) // #nosec G204,G702 -- see invariant above
	b, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		log.Printf("kubectl-ate timeout args=%q output=%q", args, strings.TrimSpace(string(b)))
		return string(b), ctx.Err()
	}
	if err != nil {
		log.Printf("kubectl-ate failed args=%q output=%q", args, strings.TrimSpace(string(b)))
		return string(b), fmt.Errorf("kubectl-ate failed")
	}
	return string(b), nil
}
