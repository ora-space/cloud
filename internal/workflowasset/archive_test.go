package workflowasset

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type archiveEntry struct {
	name string
	body string
	mode os.FileMode
}

func buildPackage(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create %s: %v", entry.name, err)
		}
		if _, err := file.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write %s: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return buffer.Bytes()
}

func requireFailure(t *testing.T, err error, code string) {
	t.Helper()
	var packageFailure *Failure
	if !errors.As(err, &packageFailure) {
		t.Fatalf("error = %v, want a package Failure", err)
	}
	if packageFailure.Code != code {
		t.Fatalf("failure code = %q, want %q", packageFailure.Code, code)
	}
}

func TestDocumentsReadsOnlyDirectWorkflowJSONChildren(t *testing.T) {
	pkg := buildPackage(t,
		archiveEntry{name: "orax.toml", body: "name = \"demo\""},
		archiveEntry{name: "README.md", body: "# demo"},
		archiveEntry{name: "assets/workflows/release.json", body: `{"name":"release"}`},
		archiveEntry{name: "assets/workflows/Deploy.JSON", body: `{"name":"deploy"}`},
		archiveEntry{name: "assets/workflows/notes.md", body: "notes"},
		archiveEntry{name: "assets/workflows/nested/inner.json", body: `{"name":"inner"}`},
		archiveEntry{name: "assets/other/ignored.json", body: `{"name":"ignored"}`},
		archiveEntry{name: `assets\workflows\windows.json`, body: `{"name":"windows"}`},
		archiveEntry{name: "/assets/workflows/absolute.json", body: `{"name":"absolute"}`},
		archiveEntry{name: "assets/workflows/directory/", mode: os.ModeDir | 0o755},
		archiveEntry{name: "assets/workflows/link.json", body: "../../escape.json", mode: os.ModeSymlink | 0o777},
	)
	documents, err := Documents(pkg)
	if err != nil {
		t.Fatalf("Documents returned %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %d, want only the two direct .json files: %#v", len(documents), documents)
	}
	if documents[0].Path != "assets/workflows/Deploy.JSON" || documents[1].Path != "assets/workflows/release.json" {
		t.Fatalf("paths = %q, %q; want a case-insensitive suffix match sorted by path", documents[0].Path, documents[1].Path)
	}
	if string(documents[1].Content) != `{"name":"release"}` {
		t.Fatalf("content = %q, want the stored entry verbatim", documents[1].Content)
	}
}

func TestDocumentsKeepsTheFirstDuplicateName(t *testing.T) {
	documents, err := Documents(buildPackage(t,
		archiveEntry{name: "assets/workflows/same.json", body: `{"name":"first"}`},
		archiveEntry{name: "assets/workflows/same.json", body: `{"name":"second"}`},
	))
	if err != nil {
		t.Fatalf("Documents returned %v", err)
	}
	if len(documents) != 1 || string(documents[0].Content) != `{"name":"first"}` {
		t.Fatalf("documents = %#v, want one deterministic first copy", documents)
	}
}

func TestDocumentsRejectsPackageLevelFailures(t *testing.T) {
	tooManyEntries := make([]archiveEntry, 0, MaxArchiveEntries+1)
	for i := 0; i <= MaxArchiveEntries; i++ {
		tooManyEntries = append(tooManyEntries, archiveEntry{name: "other/entry.txt", body: ""})
	}
	tooManyDocuments := make([]archiveEntry, 0, MaxDocuments+1)
	for i := 0; i <= MaxDocuments; i++ {
		tooManyDocuments = append(tooManyDocuments, archiveEntry{name: fmt.Sprintf("assets/workflows/doc-%03d.json", i), body: "{}"})
	}
	for _, test := range []struct {
		name    string
		pkg     []byte
		code    string
		ifReady func(t *testing.T) []byte
	}{
		{name: "not an archive", pkg: []byte("not a zip"), code: FailureInvalid},
		{name: "exceeds the package bound", pkg: make([]byte, MaxPackageBytes+1), code: FailureTooLarge},
		{name: "no documents", pkg: buildPackage(t, archiveEntry{name: "README.md", body: "# demo"}), code: FailureWithoutDocuments},
		{name: "too many entries", pkg: buildPackage(t, tooManyEntries...), code: FailureTooManyEntries},
		{name: "too many documents", pkg: buildPackage(t, tooManyDocuments...), code: FailureTooManyDocuments},
		{
			name: "expands beyond the bound",
			// An ignored entry still spends the expansion budget, so a bomb cannot hide beside
			// the documents.
			pkg:  buildPackage(t, archiveEntry{name: "other/big.bin", body: strings.Repeat("a", int(MaxExpandedBytes)+1)}),
			code: FailureExpansionTooLarge,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Documents(test.pkg)
			requireFailure(t, err, test.code)
		})
	}
}
