//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
)

// OnlineSkillProjects returns only initialized project runtimes. Configuring a market
// must never start a Node or an Agent session as a side effect.
func (d *Driver) OnlineSkillProjects() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for id, p := range d.projects {
		select {
		case <-p.ready:
			if p.initErr == nil {
				select {
				case <-p.dead:
				default:
					p.mu.Lock()
					idle := len(p.owners) == 0
					p.mu.Unlock()
					if idle {
						ids = append(ids, id)
					}
				}
			}
		default:
		}
	}
	return ids
}

var (
	ErrOfflineSkillNode = errors.New("project node offline")
	// ErrNodeRejected reports a Node receipt that did not confirm the requested plugin operation.
	ErrNodeRejected = errors.New("node rejected plugin operation")
)

func (d *Driver) ApplySkill(ctx context.Context, project, id, version, file, digest string, remove bool) error {
	d.mu.Lock()
	p := d.projects[project]
	d.mu.Unlock()
	if p == nil {
		return ErrOfflineSkillNode
	}
	select {
	case <-p.ready:
	default:
		return ErrOfflineSkillNode
	}
	if p.initErr != nil {
		return ErrOfflineSkillNode
	}
	select {
	case <-p.dead:
		return ErrOfflineSkillNode
	default:
	}
	p.skillsMu.Lock()
	defer p.skillsMu.Unlock()
	p.mu.Lock()
	// Idle reclaim may have retired the Node while this call waited for skillsMu.
	active, retired := len(p.owners) > 0, p.fatal
	p.mu.Unlock()
	if active || retired {
		return ErrOfflineSkillNode
	}
	command, outcome := "remove_plugins", "removed"
	item := map[string]any{"plugin_id": id, "version": version}
	if !remove {
		if e := pluginmarket.VerifySkillArchive(file, id, version); e != nil {
			return e
		}
		if p.services == nil {
			return ErrOfflineSkillNode
		}
		url, release, e := p.services.StageMarketPackage(nodeservices.Package{Name: "skill-" + digest, Path: file, SHA256: digest})
		if e != nil {
			return e
		}
		defer release()
		item["release"] = map[string]any{"kind": "universal", "download": map[string]string{"url": url, "sha256": digest}}
		command, outcome = "install_plugins", "installed"
	}
	msg, e := p.control(ctx, d.nextID("skill"), command, "plugins_result", map[string]any{"spec": map[string]any{"node_id": p.nodeID(), "plugins": []any{item}}})
	if e != nil {
		return e
	}
	var response struct {
		Kind   string
		Result struct {
			Items []struct {
				ID      string `json:"plugin_id"`
				Outcome struct {
					Kind    string
					Version string
					Failure string
				}
			}
		}
	}
	if json.Unmarshal(msg.Payload, &response) != nil || response.Kind != "plugins_completed" || len(response.Result.Items) != 1 {
		return fmt.Errorf("%w: node did not confirm plugin operation", ErrNodeRejected)
	}
	result := response.Result.Items[0]
	if result.ID != id || result.Outcome.Kind != outcome || !remove && result.Outcome.Version != version {
		failure := result.Outcome.Failure
		if failure == "" {
			failure = "unspecified"
		}
		return fmt.Errorf("%w: %s", ErrNodeRejected, failure)
	}
	return p.publishSkills(id, version, remove)
}
