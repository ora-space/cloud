package simulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// revisionPlan freezes artifact bytes and their declaration before the first external PUT.
// Directory is relative to the Node journal root; grants are never durable recovery evidence.
type revisionPlan struct {
	Directory   string
	Declaration json.RawMessage
}

// deliverRevision resolves the actual clone and sealed echo history once per execution. Restarts
// reuse durable artifact bytes and obtain fresh grants, including after a partially committed PUT.
func (c *Controller) deliverRevision(ctx context.Context, record *controlpb.ExecutionRecord, journal *agentJournal) (*controlpb.ExecutionResult, error) {
	if len(journal.Result) > 0 {
		result := &controlpb.ExecutionResult{}
		if err := protojson.Unmarshal(journal.Result, result); err != nil {
			return nil, fmt.Errorf("decode durable delivery evidence: %w", err)
		}
		return result, nil
	}
	result := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: record.GetNodeId(), NodeIncarnationId: "echo-fixture"}}
	failure := func(reason controlpb.RevisionFailureReason) (*controlpb.ExecutionResult, error) {
		// Controller shutdown does not terminate the Node execution. Leave its journal replayable.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result.Outcome = &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: reason}}
		return result, c.saveRevisionResult(record.GetExecutionId(), journal, result)
	}
	if journal.Delivery == nil {
		var reason revisionPreparationError
		plan, prepareErr := c.prepareRevision(ctx, record)
		if prepareErr != nil {
			if errors.As(prepareErr, &reason) {
				return failure(controlpb.RevisionFailureReason(reason))
			}
			return nil, prepareErr
		}
		journal.Delivery = plan
		if err := c.AgentNode.save(record.GetExecutionId(), journal); err != nil {
			journal.Delivery = nil
			_ = os.RemoveAll(filepath.Join(c.AgentNode.root, plan.Directory))
			return nil, err
		}
	}
	dir, err := c.revisionDirectory(journal.Delivery)
	if err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(journal.Delivery.Declaration, result); err != nil {
		return nil, fmt.Errorf("decode prepared delivery evidence: %w", err)
	}
	var history *controlpb.StoredObject
	var final string
	if delivered := result.GetRevisionDelivered(); delivered != nil {
		history, final = delivered.GetHistory(), delivered.GetFinalCommit()
	} else if unchanged := result.GetRevisionUnchanged(); unchanged != nil {
		history, final = unchanged.GetHistory(), unchanged.GetFinalCommit()
	} else {
		return nil, fmt.Errorf("invalid prepared delivery outcome")
	}
	data, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("read prepared delivery object: %w", err)
	}
	if !proto.Equal(storedBytes(history.GetKey(), data), history) {
		return nil, fmt.Errorf("prepared delivery object changed")
	}
	// The code goes to the Git Server first, unchanged runs included: the delivery ref is what Cloud
	// verifies and what a later run resumes from (Revision D1, D3).
	if reason, err := c.pushDelivery(ctx, record, final); err != nil {
		return nil, err
	} else if reason != controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UNSPECIFIED {
		return failure(reason)
	}
	if err := c.uploadRevisionObject(ctx, record.GetExecutionId(), history.GetKey(), data); err != nil {
		return failure(controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, c.saveRevisionResult(record.GetExecutionId(), journal, result)
}

func (c *Controller) prepareRevision(ctx context.Context, record *controlpb.ExecutionRecord) (*revisionPlan, error) {
	spec := record.GetInput().GetDeliverRevision()
	result := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: record.GetNodeId(), NodeIncarnationId: "echo-fixture"}}
	session, err := c.Executions.GetDispatch(ctx, &controlpb.GetDispatchRequest{ExecutionId: spec.GetSessionExecutionId()})
	if err != nil {
		return nil, err
	}
	sealed, err := c.AgentNode.load(session.GetRecord())
	if err != nil || sealed.Ended == controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_UNSPECIFIED {
		return nil, revisionPreparationError(controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_SESSION_NOT_SETTLED)
	}
	clone, err := c.Executions.GetDispatch(ctx, &controlpb.GetDispatchRequest{ExecutionId: spec.GetCheckoutExecutionId()})
	if err != nil {
		return nil, err
	}
	checkout := clone.GetRecord().GetResult().GetCloneReady()
	if checkout == nil || checkout.GetCommit() != spec.GetBaseCommit() || checkout.GetPath() == "" {
		return nil, revisionPreparationError(controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_CHECKOUT_UNAVAILABLE)
	}
	result.Node = clone.GetRecord().GetResult().GetNode()
	dir, err := os.MkdirTemp(c.AgentNode.root, "revision-")
	if err != nil {
		return nil, fmt.Errorf("create delivery scratch directory: %w", err)
	}
	prepared := false
	defer func() {
		if !prepared {
			_ = os.RemoveAll(dir)
		}
	}()
	final, err := snapshotRevision(ctx, checkout.GetPath(), dir, spec.GetRevisionRef(), session.GetRecord().GetInput().GetAgentSession().GetGitIdentity())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, revisionPreparationError(controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_SNAPSHOT_FAILED)
	}
	var history bytes.Buffer
	for _, event := range sealed.Events {
		history.WriteString(event.GetRecord())
		history.WriteByte('\n')
	}
	if err := writePreparedObject(filepath.Join(dir, "history.jsonl"), history.Bytes()); err != nil {
		return nil, err
	}
	historyObject := storedBytes(spec.GetHistoryKey(), history.Bytes())
	if final == spec.GetBaseCommit() || (spec.GetPriorRevision() != nil && final == spec.GetPriorRevision().GetFinalCommit()) {
		result.Outcome = &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{BaseCommit: spec.GetBaseCommit(), FinalCommit: final, RevisionRef: spec.GetRevisionRef(), History: historyObject}}
	} else {
		result.Outcome = &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{BaseCommit: spec.GetBaseCommit(), FinalCommit: final, RevisionRef: spec.GetRevisionRef(), History: historyObject}}
	}

	declaration, err := protojson.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode prepared delivery evidence: %w", err)
	}
	prepared = true
	return &revisionPlan{Directory: filepath.Base(dir), Declaration: declaration}, nil
}

// pushDelivery pushes the final commit to the delivery's own ref on the Git Server double, then
// tries to fast-forward the run Workspace's branch to it (contract D6). Only the delivery ref decides
// the delivery: a refused or failed branch update is ignored. A push that already landed, for example
// before a restart, finds the ref at the same commit and succeeds again.
func (c *Controller) pushDelivery(ctx context.Context, record *controlpb.ExecutionRecord, final string) (controlpb.RevisionFailureReason, error) {
	spec := record.GetInput().GetDeliverRevision()
	clone, err := c.Executions.GetDispatch(ctx, &controlpb.GetDispatchRequest{ExecutionId: spec.GetCheckoutExecutionId()})
	if err != nil {
		return 0, err
	}
	checkout := clone.GetRecord().GetResult().GetCloneReady().GetPath()
	var remote string
	ok := false
	if c.Git != nil {
		remote, ok = c.Git.Repository(spec.GetGitRepositoryUrl())
	}
	if !ok || checkout == "" {
		return controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_PUSH_REJECTED, nil
	}
	if _, err := git(ctx, "-C", checkout, "push", "--quiet", "--", remote, final+":"+spec.GetDeliveryRef()); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_PUSH_FAILED, nil
	}
	_, _ = git(ctx, "-C", checkout, "push", "--quiet", "--", remote, final+":"+spec.GetWorkspaceRef())
	return controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UNSPECIFIED, nil
}

func (c *Controller) saveRevisionResult(execution string, journal *agentJournal, result *controlpb.ExecutionResult) error {
	data, err := protojson.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode durable delivery evidence: %w", err)
	}
	next := *journal
	next.Result = data
	next.Delivery = nil
	if err := c.AgentNode.save(execution, &next); err != nil {
		return err
	}
	if journal.Delivery != nil {
		if dir, err := c.revisionDirectory(journal.Delivery); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
	*journal = next
	return nil
}

func (c *Controller) revisionDirectory(plan *revisionPlan) (string, error) {
	if !strings.HasPrefix(plan.Directory, "revision-") || filepath.Base(plan.Directory) != plan.Directory {
		return "", fmt.Errorf("invalid delivery artifact directory")
	}
	return filepath.Join(c.AgentNode.root, plan.Directory), nil
}

// Preparation errors carry a bounded Node failure without exposing Git or filesystem output.
type revisionPreparationError controlpb.RevisionFailureReason

func (e revisionPreparationError) Error() string { return controlpb.RevisionFailureReason(e).String() }

func writePreparedObject(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open prepared delivery object: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write prepared delivery object: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync prepared delivery object: %w", err)
	}
	return file.Close()
}

func storedBytes(key string, data []byte) *controlpb.StoredObject {
	sum := sha256.Sum256(data)
	return &controlpb.StoredObject{Key: key, Size: uint64(len(data)), Sha256: hex.EncodeToString(sum[:])}
}

// snapshotRevision uses a scratch index, never the user's index, branch or commit hooks.
func snapshotRevision(ctx context.Context, checkout, scratch, ref string, identity *controlpb.GitIdentity) (string, error) {
	if !strings.HasPrefix(ref, "refs/ora/revisions/") {
		return "", fmt.Errorf("invalid revision ref")
	}
	if _, err := git(ctx, "-C", checkout, "check-ref-format", ref); err != nil {
		return "", err
	}
	head, err := git(ctx, "-C", checkout, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	status, err := git(ctx, "-C", checkout, "status", "--porcelain=v2", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	final := head
	if status != "" {
		run := func(args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", checkout, "-c", "core.hooksPath="}, args...)...) // #nosec G204 -- simulator-only Git, clone-owned path and fixed plumbing arguments below.
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"), "GIT_AUTHOR_NAME="+identity.GetName(), "GIT_AUTHOR_EMAIL="+identity.GetEmail(), "GIT_COMMITTER_NAME=Ora", "GIT_COMMITTER_EMAIL=revision@ora.invalid")
			output, err := cmd.Output()
			return strings.TrimSpace(string(output)), err
		}
		if _, err = run("read-tree", "HEAD"); err != nil {
			return "", err
		}
		if _, err = run("add", "-A"); err != nil {
			return "", err
		}
		tree, err := run("write-tree")
		if err != nil {
			return "", err
		}
		final, err = run("commit-tree", tree, "-p", head, "-m", "Ora internal snapshot of uncommitted run changes")
		if err != nil {
			return "", err
		}
	}
	_, err = git(ctx, "-C", checkout, "update-ref", ref, final)
	return final, err
}

func (c *Controller) uploadRevisionObject(ctx context.Context, execution, key string, data []byte) error {
	sum := sha256.Sum256(data)
	for range 3 {
		if err := ctx.Err(); err != nil {
			return err
		}
		response, err := c.AgentRuns.GrantRevisionUpload(ctx, &controlpb.GrantRevisionUploadRequest{Epoch: c.Epoch, ExecutionId: execution, Checksums: map[string]string{key: hex.EncodeToString(sum[:])}})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("delivery grant unavailable")
		}
		for _, grant := range response.GetGrants() {
			if grant.GetObjectKey() != key || !grant.GetExpiresAt().AsTime().After(time.Now()) {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, grant.GetMethod(), grant.GetUrl(), bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("delivery upload request invalid")
			}
			for name, value := range grant.GetHeaders() {
				req.Header.Set(name, value)
			}
			req.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
			client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if ctx.Err() != nil {
				if resp != nil {
					_ = resp.Body.Close()
				}
				return ctx.Err()
			}
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				// A previous PUT may have committed while its response was lost. Do not overwrite it
				// or infer matching bytes: submit the declaration for Cloud's authoritative HEAD.
				if resp.StatusCode == http.StatusPreconditionFailed {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("delivery upload failed")
}
