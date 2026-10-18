package workflowasset

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Documents reads the package's shallow assets/workflows/*.json entries. It applies the approved
// archive bounds before touching any entry: entry count ≤ 512, expanded total ≤ 64 MiB, documents
// 1..=256. Only direct children are candidates; READMEs, other extensions, subdirectories and
// symbolic links are ignored rather than rejected. The returned slice is sorted by document path,
// so one plugin version always produces the same document set in the same order.
func Documents(pkg []byte) ([]Document, error) {
	if int64(len(pkg)) > MaxPackageBytes {
		return nil, failure(FailureTooLarge, "package exceeds the bound")
	}
	reader, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return nil, failure(FailureInvalid, "package is not a readable archive")
	}
	if len(reader.File) > MaxArchiveEntries {
		return nil, failure(FailureTooManyEntries, "archive has too many entries")
	}
	type candidate struct {
		path string
		file *zip.File
	}
	var candidates []candidate
	var expanded uint64
	seen := map[string]bool{}
	for _, file := range reader.File {
		if !isDocumentEntry(file) {
			// Ignored entries still count toward the expansion budget: a package cannot smuggle a
			// zip bomb past the bound by hiding it beside the documents.
			expanded += file.UncompressedSize64
			if expanded > uint64(MaxExpandedBytes) {
				return nil, failure(FailureExpansionTooLarge, "archive expands beyond the bound")
			}
			continue
		}
		expanded += file.UncompressedSize64
		if expanded > uint64(MaxExpandedBytes) {
			return nil, failure(FailureExpansionTooLarge, "archive expands beyond the bound")
		}
		if seen[file.Name] {
			// A duplicate name contributes one path; keeping the first is deterministic.
			continue
		}
		seen[file.Name] = true
		candidates = append(candidates, candidate{path: file.Name, file: file})
	}
	if len(candidates) == 0 {
		return nil, failure(FailureWithoutDocuments, "package carries no workflow documents")
	}
	if len(candidates) > MaxDocuments {
		return nil, failure(FailureTooManyDocuments, "package carries too many workflow documents")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })
	documents := make([]Document, 0, len(candidates))
	remaining := MaxExpandedBytes
	for _, c := range candidates {
		content, e := readEntry(c.file, remaining)
		if e != nil {
			return nil, e
		}
		remaining -= int64(len(content))
		documents = append(documents, Document{Path: c.path, Content: content})
	}
	return documents, nil
}

// isDocumentEntry applies the package layout contract to one archive entry. Symbolic links are
// ignored without being followed, so they can neither be imported nor redirect a read outside the
// package root.
func isDocumentEntry(file *zip.File) bool {
	if file.FileInfo().IsDir() {
		return false
	}
	if file.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	name := file.Name
	if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return false
	}
	if path.Dir(name) != DocumentDirectory {
		return false
	}
	base := path.Base(name)
	if base == "." || base == ".." || base == "" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(base), ".json")
}

// readEntry reads one entry under the remaining expansion budget. The header's declared size is
// only a hint, so the read itself is bounded; a mismatch means the archive lied and is refused.
func readEntry(file *zip.File, limit int64) ([]byte, error) {
	body, err := file.Open()
	if err != nil {
		return nil, failure(FailureInvalid, "archive entry is not readable")
	}
	defer func() { _ = body.Close() }()
	content, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, failure(FailureInvalid, "archive entry is not readable")
	}
	if int64(len(content)) > limit {
		return nil, failure(FailureExpansionTooLarge, "archive expands beyond the bound")
	}
	return content, nil
}
