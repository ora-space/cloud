package gitread

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/internal/mirror"
)

const (
	testRepository = "https://github.com/ora-space/demo"
	testCommit     = "1234567890abcdef1234567890abcdef12345678"
)

func key() mirror.Key {
	return mirror.Key{ProjectID: "project-1", Repository: testRepository, Commit: testCommit}
}

func origin() Origin {
	return Origin{WorkspaceID: "workspace-1", ContainerID: "container-1", ContainerName: "ora-workspace-1"}
}

// allowed is the snapshot that must produce an allow, so each case can change exactly one thing and
// the refusal it produces can only be attributed to that change.
func allowed() Snapshot {
	return Snapshot{
		Found:              true,
		Key:                key(),
		MirrorGeneration:   2,
		MirrorReady:        true,
		ContainerLive:      true,
		WorkspaceID:        "workspace-1",
		ContainerID:        "container-1",
		RuntimeGeneration:  3,
		WorkspaceProjectID: "project-1",
		WorkspaceTenantID:  "tenant-1",
		ProjectTenantID:    "tenant-1",
	}
}

func request() Request {
	return Request{Origin: origin(), MirrorDigest: key().Digest(), Operation: OperationUploadPack}
}

func TestEvaluateAllowsTheAuthorizedRead(t *testing.T) {
	verdict := request().Evaluate(allowed())
	if !verdict.Allow {
		t.Fatalf("an authorized read was refused with %q", verdict.Reason)
	}
	if verdict.Reason != "" {
		t.Fatalf("an allow carries a refusal reason %q", verdict.Reason)
	}
	if verdict.Key != key() {
		t.Fatalf("verdict key = %+v, want %+v", verdict.Key, key())
	}
	if verdict.Generation != 2 {
		t.Fatalf("verdict generation = %d, want the authorized 2", verdict.Generation)
	}
}

func TestEvaluateRefusesEveryIncompleteOrInconsistentState(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Request)
		snapshot func(*Snapshot)
		reason   string
	}{
		{
			name:   "receive-pack",
			mutate: func(r *Request) { r.Operation = OperationReceivePack },
			reason: ReasonOperationNotServed,
		},
		{
			name:   "upload-archive",
			mutate: func(r *Request) { r.Operation = OperationUploadArchive },
			reason: ReasonOperationNotServed,
		},
		{
			name:   "unknown service",
			mutate: func(r *Request) { r.Operation = OperationUnknown },
			reason: ReasonOperationNotServed,
		},
		{
			name:   "empty service",
			mutate: func(r *Request) { r.Operation = "" },
			reason: ReasonOperationNotServed,
		},
		{
			name:   "origin without a workspace",
			mutate: func(r *Request) { r.Origin.WorkspaceID = " " },
			reason: ReasonOriginIncomplete,
		},
		{
			name:   "origin without a container",
			mutate: func(r *Request) { r.Origin.ContainerID = "" },
			reason: ReasonOriginIncomplete,
		},
		{
			name:   "no mirror named",
			mutate: func(r *Request) { r.MirrorDigest = "" },
			reason: ReasonMirrorUnknown,
		},
		{
			name:     "cloud knows no such mirror",
			snapshot: func(s *Snapshot) { s.Found = false },
			reason:   ReasonMirrorUnknown,
		},
		{
			name:     "cloud answered with a key that is not usable",
			snapshot: func(s *Snapshot) { s.Key.Commit = "short" },
			reason:   ReasonMirrorUnknown,
		},
		{
			name:     "the digest names a different key",
			snapshot: func(s *Snapshot) { s.Key.Commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
			reason:   ReasonDigestMismatch,
		},
		{
			name:     "the container is not the live one",
			snapshot: func(s *Snapshot) { s.ContainerLive = false },
			reason:   ReasonContainerNotLive,
		},
		{
			name:     "the workspace and the project disagree about the tenant",
			snapshot: func(s *Snapshot) { s.ProjectTenantID = "tenant-2" },
			reason:   ReasonCrossTenant,
		},
		{
			name:     "cloud did not report the workspace's tenant",
			snapshot: func(s *Snapshot) { s.WorkspaceTenantID = "" },
			reason:   ReasonCrossTenant,
		},
		{
			name:     "cloud did not report the project's tenant",
			snapshot: func(s *Snapshot) { s.ProjectTenantID = "" },
			reason:   ReasonCrossTenant,
		},
		{
			name:     "cross workspace",
			snapshot: func(s *Snapshot) { s.WorkspaceID = "workspace-2" },
			reason:   ReasonCrossWorkspace,
		},
		{
			name:     "a different container of the same workspace",
			snapshot: func(s *Snapshot) { s.ContainerID = "container-2" },
			reason:   ReasonContainerNotLive,
		},
		{
			name: "a container from an earlier runtime generation",
			// An earlier generation is a different sandbox instance row, so it fails as a stale
			// container rather than as a comparison of a number the caller could assert.
			snapshot: func(s *Snapshot) { s.RuntimeGeneration = 2; s.ContainerLive = false },
			reason:   ReasonContainerNotLive,
		},
		{
			name:     "the workspace belongs to another project",
			snapshot: func(s *Snapshot) { s.WorkspaceProjectID = "project-2" },
			reason:   ReasonCrossProject,
		},
		{
			name:     "the workspace belongs to no project",
			snapshot: func(s *Snapshot) { s.WorkspaceProjectID = "" },
			reason:   ReasonCrossProject,
		},
		{
			name:     "the mirror was withdrawn",
			snapshot: func(s *Snapshot) { s.MirrorRevoked = true },
			reason:   ReasonMirrorRevoked,
		},
		{
			name:     "the mirror is not ready",
			snapshot: func(s *Snapshot) { s.MirrorReady = false },
			reason:   ReasonMirrorNotReady,
		},
		{
			name:     "the mirror has no generation",
			snapshot: func(s *Snapshot) { s.MirrorGeneration = 0 },
			reason:   ReasonMirrorNotReady,
		},
		{
			name:     "the mirror is both ready and withdrawn",
			snapshot: func(s *Snapshot) { s.MirrorRevoked = true },
			reason:   ReasonMirrorRevoked,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := request()
			snapshot := allowed()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			if tc.snapshot != nil {
				tc.snapshot(&snapshot)
			}
			verdict := req.Evaluate(snapshot)
			if verdict.Allow {
				t.Fatalf("%s was allowed", tc.name)
			}
			if verdict.Reason != tc.reason {
				t.Fatalf("%s refused with %q, want %q", tc.name, verdict.Reason, tc.reason)
			}
			if verdict.Key != (mirror.Key{}) || verdict.Generation != 0 {
				t.Fatalf("%s returned a half-filled verdict: %+v", tc.name, verdict)
			}
		})
	}
}

func TestEvaluateRefusesARevokedMirrorEvenWhenEveryBindingMatches(t *testing.T) {
	// Revocation is not a consequence of the binding: a request whose container, workspace, tenant
	// binding and project are all exactly right is still refused once the mirror is withdrawn.
	snapshot := allowed()
	snapshot.MirrorRevoked = true
	if verdict := request().Evaluate(snapshot); verdict.Allow {
		t.Fatal("a revoked mirror was served to a correctly bound caller")
	}
}

func TestOriginValidateRejectsAnIncompleteIdentity(t *testing.T) {
	complete := origin()
	if err := complete.Validate(); err != nil {
		t.Fatalf("a complete origin was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Origin){
		"workspace": func(o *Origin) { o.WorkspaceID = "" },
		"blank":     func(o *Origin) { o.WorkspaceID = " " },
		"container": func(o *Origin) { o.ContainerID = "" },
	} {
		o := complete
		mutate(&o)
		err := o.Validate()
		if !errors.Is(err, ErrOrigin) {
			t.Fatalf("%s: Validate = %v, want ErrOrigin", name, err)
		}
	}
}

func TestParseServiceRecognisesOnlyTheGitServices(t *testing.T) {
	cases := map[string]Operation{
		"git-upload-pack":    OperationUploadPack,
		"git-receive-pack":   OperationReceivePack,
		"git-upload-archive": OperationUploadArchive,
		"":                   OperationUnknown,
		"git-upload-pack ":   OperationUnknown,
		"upload-pack":        OperationUnknown,
		"git://evil":         OperationUnknown,
	}
	for service, want := range cases {
		if got := ParseService(service); got != want {
			t.Fatalf("ParseService(%q) = %q, want %q", service, got, want)
		}
	}
	for path, want := range map[string]Operation{
		"git-upload-pack":    OperationUploadPack,
		"git-receive-pack":   OperationReceivePack,
		"git-upload-archive": OperationUploadArchive,
		"info/refs":          OperationUnknown,
	} {
		if got := ParseRPCPath(path); got != want {
			t.Fatalf("ParseRPCPath(%q) = %q, want %q", path, got, want)
		}
	}
}
