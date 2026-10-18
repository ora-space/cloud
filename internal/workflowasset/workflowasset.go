// Package workflowasset reads the workflow documents a kind='workflow' plugin package delivers.
// Cloud downloads the package itself, verifies its SHA-256, extracts only the shallow
// assets/workflows/*.json documents, and leaves parsing to the caller so one bad document never
// aborts the rest. The package never touches PostgreSQL and holds no package bytes beyond one call.
package workflowasset

import "fmt"

// Package bounds. They are the approved values in
// specs/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md D3: the archive cap
// fails early, while the expanded cap and entry cap are the real zip-bomb controls.
const (
	MaxPackageBytes   int64 = 16 << 20
	MaxExpandedBytes  int64 = 64 << 20
	MaxArchiveEntries       = 512
	MaxDocuments            = 256
	DocumentDirectory       = "assets/workflows"
)

// Bounded package-level failure codes. Download, verification and extraction failures belong to
// the package as a whole and produce no document results; the next trigger retries them.
const (
	FailureUnavailable       = "asset_package_unavailable"
	FailureTooLarge          = "asset_package_too_large"
	FailureDigestMismatch    = "asset_package_digest_mismatch"
	FailureInvalid           = "asset_package_invalid"
	FailureExpansionTooLarge = "asset_package_expansion_too_large"
	FailureTooManyEntries    = "asset_package_too_many_entries"
	FailureWithoutDocuments  = "asset_package_without_documents"
	FailureTooManyDocuments  = "asset_package_too_many_documents"
)

// Bounded document-level failure codes produced before any database write.
const (
	DocumentInvalidJSON = "document_invalid_json"
	DocumentWithoutName = "document_without_name"
)

// Failure is a bounded package-level error. Detail is for structured logs only and never reaches
// the stored source record.
type Failure struct {
	Code   string
	Detail string
}

func (f *Failure) Error() string {
	if f.Detail == "" {
		return f.Code
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Detail)
}

func failure(code, detail string) *Failure { return &Failure{Code: code, Detail: detail} }

// Document is one shallow assets/workflows/*.json entry, already bounded and read into memory.
type Document struct {
	Path    string
	Content []byte
}

// DocumentError is a bounded document-level parse failure; Reason is a stable identifier safe to
// persist beside the code.
type DocumentError struct {
	Code   string
	Reason string
}

func (e *DocumentError) Error() string { return e.Code + ": " + e.Reason }
