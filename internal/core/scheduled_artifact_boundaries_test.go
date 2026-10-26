package core

import (
	"strings"
	"testing"
)

const emptyArtifactSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestScheduledArtifactManifestAcceptsEmptyFile(t *testing.T) {
	manifest := Object{"items": []Object{{"index": 0, "displayName": "empty.txt", "kind": "text", "sizeBytes": 0, "sha256": emptyArtifactSHA256}}}
	declared, err := parseScheduledArtifactManifest("tenant", "run", manifest)
	if err != nil || len(declared.Artifacts) != 1 || declared.Artifacts[0].Size != 0 {
		t.Fatalf("empty file declaration = %+v, %v", declared, err)
	}
}

func TestScheduledArtifactManifestRejectsDuplicateIndex(t *testing.T) {
	for _, digest := range []string{strings.Repeat("ab", 32), strings.Repeat("cd", 32)} {
		manifest := Object{"items": []Object{
			{"index": 0, "displayName": "first.txt", "kind": "text", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32)},
			{"index": 0, "displayName": "second.txt", "kind": "text", "sizeBytes": 5, "sha256": digest},
		}}
		if _, err := parseScheduledArtifactManifest("tenant", "run", manifest); err == nil {
			t.Fatal("different names must not share an artifact index, regardless of digest")
		}
	}
}
