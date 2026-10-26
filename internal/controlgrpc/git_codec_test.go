package controlgrpc

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

const (
	codecTenant    = "6f9dd12a-3dd0-42c7-88cd-41c21bfa748e"
	codecProject   = "e93c0c7d-b434-4cdb-ba1a-d1f519fe28e8"
	codecBinding   = "46722143-1acd-4fa2-a3cf-b510457795f5"
	codecWorkspace = "bf8abde8-edd6-4505-be20-05728658c7de"
	codecWork      = "0b2911dd-6688-4051-b0f0-5e35f14594e1"
)

func validClone() *controlpb.CloneSpec {
	commit := strings.Repeat("c", 40)
	return &controlpb.CloneSpec{
		RepositoryUrl: "https://git-server:8445/" + codecTenant + "/" + codecProject + ".git",
		SnapshotRef:   core.GitSnapshotRef(codecBinding, commit),
		BaseCommit:    commit,
		WorkspaceRef:  core.GitWorkspaceRefPrefix(codecWorkspace) + "main",
	}
}

// Contract D1: a Workspace clone input is the four snapshot fields, round-tripped verbatim into the
// durable shape {kind, repositoryUrl, snapshotRef, baseCommit, workspaceRef}.
func TestCloneSpecRoundTripsTheSnapshotInput(t *testing.T) {
	spec := validClone()
	got, err := inputObject(&controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: spec}})
	if err != nil {
		t.Fatal(err)
	}
	want := core.Object{"kind": "clone", "repositoryUrl": spec.GetRepositoryUrl(), "snapshotRef": spec.GetSnapshotRef(), "baseCommit": spec.GetBaseCommit(), "workspaceRef": spec.GetWorkspaceRef()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable clone input = %v, want %v", got, want)
	}
	if back := input(got).GetClone(); !proto.Equal(back, spec) {
		t.Fatalf("clone input changed across the codec: %v", back)
	}
}

// Contract D1: every field is required and shaped; in particular the snapshot ref must name the base
// commit, so a Controller cannot pair a snapshot with another commit.
func TestCloneSpecRejectsMalformedInput(t *testing.T) {
	other := strings.Repeat("d", 40)
	for name, edit := range map[string]func(*controlpb.CloneSpec){
		"empty repository": func(s *controlpb.CloneSpec) { s.RepositoryUrl = "" },
		"http repository":  func(s *controlpb.CloneSpec) { s.RepositoryUrl = strings.Replace(s.RepositoryUrl, "https", "http", 1) },
		"repository with userinfo": func(s *controlpb.CloneSpec) {
			s.RepositoryUrl = strings.Replace(s.RepositoryUrl, "https://", "https://user@", 1)
		},
		"repository with query":       func(s *controlpb.CloneSpec) { s.RepositoryUrl += "?x=1" },
		"repository with fragment":    func(s *controlpb.CloneSpec) { s.RepositoryUrl += "#x" },
		"snapshot of another commit":  func(s *controlpb.CloneSpec) { s.BaseCommit = other },
		"snapshot outside upstreams":  func(s *controlpb.CloneSpec) { s.SnapshotRef = "refs/heads/main" },
		"tracking ref as snapshot":    func(s *controlpb.CloneSpec) { s.SnapshotRef = core.GitUpstreamHeadPrefix(codecBinding) + "main" },
		"uppercase commit":            func(s *controlpb.CloneSpec) { s.BaseCommit = strings.ToUpper(s.BaseCommit) },
		"workspace ref outside":       func(s *controlpb.CloneSpec) { s.WorkspaceRef = "refs/heads/main" },
		"workspace branch HEAD":       func(s *controlpb.CloneSpec) { s.WorkspaceRef = core.GitWorkspaceRefPrefix(codecWorkspace) + "HEAD" },
		"workspace ref without owner": func(s *controlpb.CloneSpec) { s.WorkspaceRef = "refs/heads/workspaces/main" },
		"empty workspace ref":         func(s *controlpb.CloneSpec) { s.WorkspaceRef = "" },
	} {
		spec := validClone()
		edit(spec)
		_, err := inputObject(&controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: spec}})
		if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || detailOf(st) != controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT {
			t.Errorf("%s: err = %v, want InvalidArgument with INVALID_INPUT", name, err)
		}
	}
}

// Revision D1 and contract D6: a delivery input carries the Git Server repository, its own delivery
// ref and the Workspace branch, and no bundle key; all three are required and shaped.
func TestDeliverySpecCarriesGitLocations(t *testing.T) {
	valid := func() *controlpb.DeliverRevisionSpec {
		return &controlpb.DeliverRevisionSpec{
			SessionExecutionId: "session-1", CheckoutExecutionId: "checkout-1", BaseCommit: strings.Repeat("c", 40),
			RevisionRef: "refs/ora/revisions/run-1", HistoryKey: "revisions/t/r/w/session.jsonl",
			GitRepositoryUrl: "https://git-server:8445/" + codecTenant + "/" + codecProject + ".git",
			DeliveryRef:      core.GitDeliveryRefPrefix + codecWork, WorkspaceRef: core.GitWorkspaceRefPrefix(codecWorkspace) + "main",
			PriorRevision: &controlpb.PriorRevision{RevisionId: "r", FinalCommit: strings.Repeat("e", 40)},
		}
	}
	spec := valid()
	got, err := deliveryObject(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.S("deliveryRef") != spec.GetDeliveryRef() || got.S("workspaceRef") != spec.GetWorkspaceRef() || got.S("gitRepositoryUrl") != spec.GetGitRepositoryUrl() || got["bundleKey"] != nil {
		t.Fatalf("durable delivery input = %v", got)
	}
	if prior := got.O("priorRevision"); len(prior) != 2 {
		t.Fatalf("a delivery's prior carries identity and final commit only: %v", prior)
	}
	if back := deliveryMessage(got); !proto.Equal(back, spec) {
		t.Fatalf("delivery input changed across the codec: %v", back)
	}
	for name, edit := range map[string]func(*controlpb.DeliverRevisionSpec){
		"no repository":           func(s *controlpb.DeliverRevisionSpec) { s.GitRepositoryUrl = "" },
		"no delivery ref":         func(s *controlpb.DeliverRevisionSpec) { s.DeliveryRef = "" },
		"delivery ref elsewhere":  func(s *controlpb.DeliverRevisionSpec) { s.DeliveryRef = "refs/heads/main" },
		"no workspace ref":        func(s *controlpb.DeliverRevisionSpec) { s.WorkspaceRef = "" },
		"workspace ref elsewhere": func(s *controlpb.DeliverRevisionSpec) { s.WorkspaceRef = core.GitDeliveryRefPrefix + codecWork },
		"no history key":          func(s *controlpb.DeliverRevisionSpec) { s.HistoryKey = "" },
		"no session":              func(s *controlpb.DeliverRevisionSpec) { s.SessionExecutionId = "" },
		"repository without .git": func(s *controlpb.DeliverRevisionSpec) {
			s.GitRepositoryUrl = "https://git-server:8445/" + codecTenant + "/" + codecProject
		},
		"repository with userinfo": func(s *controlpb.DeliverRevisionSpec) {
			s.GitRepositoryUrl = "https://u:p@git-server/" + codecTenant + "/" + codecProject + ".git"
		},
	} {
		spec := valid()
		edit(spec)
		if _, err := deliveryObject(spec); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
}

// Revision D6: a delivered result has no bundle on the wire any more.
func TestRevisionDeliveredCarriesOnlyTheHistory(t *testing.T) {
	res := &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: "n", NodeIncarnationId: "i"},
		Outcome: &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{
			FinalCommit: strings.Repeat("f", 40), BaseCommit: strings.Repeat("c", 40), RevisionRef: "refs/ora/revisions/r",
			History: &controlpb.StoredObject{Key: "k", Size: 1, Sha256: strings.Repeat("a", 64)},
		}},
	}
	got, err := resultObject(res)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["bundle"]; ok {
		t.Fatalf("a delivered result decoded a bundle: %v", got)
	}
	if back := result(got); !proto.Equal(back, res) {
		t.Fatalf("result changed across the codec: %v", back)
	}
}
