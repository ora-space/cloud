package core

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// WorkflowAssetSource downloads one package revision selected by the catalog snapshot. It is
// defined here, at the consuming boundary, so tests substitute a local package and production
// wires the checked HTTPS/object-store fetcher.
type WorkflowAssetSource interface {
	Fetch(ctx context.Context, locator, sha256 string) ([]byte, error)
}

// assetImportLease lets a crashed import be reclaimed. The worker does no external work inside a
// transaction, so only a process death can leave a claimed row behind.
const assetImportLease = 15 * time.Minute

// assetImportBatch bounds one tick's downloads. The loop owns no queue: every tick rescans the
// small set of installed workflow plugins.
const assetImportBatch = 8

// requestWorkflowAssetImport records the administrator trigger in the install transaction. The
// revision bump is the retry signal: repeated installs of the same version retry failed documents,
// and a new version retries every path that has not yet imported successfully. The row is written
// before any external work, so a crash still leaves the request recoverable.
func requestWorkflowAssetImport(t *transaction, spaceID, tenantID, namespace, identifier string, entry Object) {
	if entry.S("kind") != "workflow" {
		return
	}
	t.exec(`INSERT INTO workflow_asset_imports(space_id,tenant_id,source_namespace,identifier,package_version,package_sha256)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (space_id,source_namespace,identifier) DO UPDATE SET
		 requested_revision=workflow_asset_imports.requested_revision+1,
		 package_version=EXCLUDED.package_version,
		 package_sha256=EXCLUDED.package_sha256,
		 state='pending',failure_code=NULL,lease_until=NULL,completed_at=NULL,
		 version=workflow_asset_imports.version+1,updated_at=now()`,
		spaceID, tenantID, namespace, identifier, entry.S("version"), entry.S("sha256"))
}

// workflowAssetJob is one due attempt resolved against the selection's pinned release. The locator
// is never stored in the source record: it comes from the selected catalog entry at claim time.
type workflowAssetJob struct {
	SpaceID, TenantID, Namespace, Identifier string
	PackageVersion, PackageSHA256, Locator   string
	// RequestedRevision is the revision the job was resolved against. The claim compares it with the
	// row's current revision, so an administrator's reinstall between the scan and the claim makes
	// the job stale rather than letting it download the previously selected package under the newer
	// revision's fence.
	RequestedRevision int64
}

// dueWorkflowAssetImports returns at most one batch of pending or lease-expired attempts. A
// failed attempt waits for the next administrator trigger; a crash-expired one is retried without
// a new trigger because the previous attempt never recorded a result.
func (s *Store) dueWorkflowAssetImports(ctx context.Context) ([]workflowAssetJob, error) {
	out, err := s.transact(ctx, func(t *transaction) Object {
		return Object{"rows": t.list(`SELECT i.space_id,i.tenant_id,i.source_namespace,i.identifier,
			 i.requested_revision AS requested_revision,
			 coalesce(nullif(p.selected_release->>'version',''),c.version,i.package_version) AS package_version,
			 coalesce(nullif(p.selected_release->>'sha256',''),c.sha256,i.package_sha256) AS package_sha256,
			 coalesce(nullif(p.selected_release->>'url',''),c.url,'') AS locator
			FROM workflow_asset_imports i
			JOIN space_plugins p
			  ON p.space_id=i.space_id AND p.source_namespace=i.source_namespace AND p.identifier=i.identifier
			LEFT JOIN plugin_catalog_entries c
			  ON c.source_namespace=i.source_namespace AND c.identifier=i.identifier AND c.version=p.desired_version
			WHERE p.desired_state='installed'
			  AND ((i.state='pending' AND i.attempted_revision<i.requested_revision)
			       OR (i.state='importing' AND i.lease_until<now()))
			ORDER BY i.updated_at,i.space_id LIMIT $1`, assetImportBatch)}
	})
	if err != nil {
		return nil, err
	}
	rows, _ := out["rows"].([]Object)
	jobs := make([]workflowAssetJob, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, workflowAssetJob{
			SpaceID: row.S("spaceId"), TenantID: row.S("tenantId"),
			Namespace: row.S("sourceNamespace"), Identifier: row.S("identifier"),
			PackageVersion: row.S("packageVersion"), PackageSHA256: row.S("packageSha256"),
			Locator: row.S("locator"), RequestedRevision: row.N("requestedRevision"),
		})
	}
	return jobs, nil
}

// claimWorkflowAssetImport moves one due row to importing and returns the claimed revision, or nil
// when the claim is stale or another worker got there first. The revision is the fencing value both
// the job's package identity and the result write are checked against: a reinstall between the scan
// and this claim bumps requested_revision, the CAS below matches nothing, and the next pass resolves
// the job again from the newer selection.
func (s *Store) claimWorkflowAssetImport(ctx context.Context, job *workflowAssetJob) (int64, error) {
	out, err := s.transact(ctx, func(t *transaction) Object {
		changed := t.execRows(`UPDATE workflow_asset_imports SET state='importing',
			 attempted_revision=requested_revision,attempted_at=now(),
			 package_version=$4,package_sha256=$5,
			 lease_until=now()+$6::interval,
			 version=version+1,updated_at=now()
			WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3
			  AND requested_revision=$7
			  AND ((state='pending' AND attempted_revision<requested_revision)
			       OR (state='importing' AND lease_until<now()))`,
			job.SpaceID, job.Namespace, job.Identifier, job.PackageVersion, job.PackageSHA256,
			fmt.Sprintf("%d seconds", int(assetImportLease/time.Second)), job.RequestedRevision)
		if changed == 0 {
			return nil
		}
		return Object{"revision": job.RequestedRevision}
	})
	if err != nil {
		return 0, err
	}
	if out == nil {
		return 0, nil
	}
	return out.N("revision"), nil
}

// workflowAssetParseResult carries the offline work: parsing and graph validation happen before
// the transaction that writes results.
type workflowAssetParseResult struct {
	Document workflowasset.Document
	Parsed   workflowasset.ParsedDocument
	Failure  string
	Reason   string
	Problems []any
}

// ImportWorkflowAssetsOnce performs one bounded pass: claim due attempts, download and extract
// outside any transaction, then write every result in one short transaction. Package-level
// failures are recorded, not retried here; they wait for the next administrator trigger. Errors
// returned from this method are infrastructure failures or recorded package failures, for the
// process loop to log. Selections installed before this feature existed are deliberately not
// backfilled: the decision covers imports triggered after it takes effect, and a reinstall is the
// administrator's explicit trigger.
func (s *Store) ImportWorkflowAssetsOnce(ctx context.Context) error {
	if s.WorkflowAssets == nil {
		return nil
	}
	jobs, err := s.dueWorkflowAssetImports(ctx)
	if err != nil {
		return fmt.Errorf("select workflow asset imports: %w", err)
	}
	var failures []error
	for i := range jobs {
		job := &jobs[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		revision, err := s.claimWorkflowAssetImport(ctx, job)
		if err != nil {
			failures = append(failures, fmt.Errorf("claim %s/%s for %s: %w", job.Namespace, job.Identifier, job.SpaceID, err))
			continue
		}
		if revision == 0 {
			continue
		}
		if job.Locator == "" || job.PackageSHA256 == "" {
			if err := s.finishWorkflowAssetImport(ctx, job, revision, nil, workflowasset.FailureUnavailable); err != nil {
				failures = append(failures, err)
			}
			failures = append(failures, fmt.Errorf("%s/%s for %s: package locator is missing", job.Namespace, job.Identifier, job.SpaceID))
			continue
		}
		pkg, fetchErr := s.WorkflowAssets.Fetch(ctx, job.Locator, job.PackageSHA256)
		if fetchErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			code := assetFailureCode(fetchErr)
			if err := s.finishWorkflowAssetImport(ctx, job, revision, nil, code); err != nil {
				failures = append(failures, err)
			}
			failures = append(failures, fmt.Errorf("%s/%s for %s: %s", job.Namespace, job.Identifier, job.SpaceID, code))
			continue
		}
		documents, extractErr := workflowasset.Documents(pkg)
		if extractErr != nil {
			code := assetFailureCode(extractErr)
			if err := s.finishWorkflowAssetImport(ctx, job, revision, nil, code); err != nil {
				failures = append(failures, err)
			}
			failures = append(failures, fmt.Errorf("%s/%s for %s: %s", job.Namespace, job.Identifier, job.SpaceID, code))
			continue
		}
		results := make([]workflowAssetParseResult, 0, len(documents))
		for _, document := range documents {
			results = append(results, parseWorkflowAssetDocument(document))
		}
		if err := s.finishWorkflowAssetImport(ctx, job, revision, results, ""); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// parseWorkflowAssetDocument does the CPU-only work: JSON parsing, name extraction and the shared
// publish gate. The gate's problems become the document's bounded failure detail.
func parseWorkflowAssetDocument(document workflowasset.Document) workflowAssetParseResult {
	result := workflowAssetParseResult{Document: document}
	parsed, err := workflowasset.Parse(document.Content)
	if err != nil {
		var documentErr *workflowasset.DocumentError
		if errors.As(err, &documentErr) {
			result.Failure, result.Reason = documentErr.Code, documentErr.Reason
		} else {
			result.Failure, result.Reason = workflowasset.DocumentInvalidJSON, "malformed_json"
		}
		return result
	}
	// The document is stored verbatim in a jsonb column. PostgreSQL rejects text and numbers that
	// Go's decoder accepts, so such a document must fail alone instead of aborting the transaction
	// that writes every other document's result.
	if reason, unsupported := postgresUnsupportedJSON(document.Content); unsupported {
		result.Failure, result.Reason = workflowasset.DocumentInvalidJSON, reason
		return result
	}
	problems := validateWorkflowGraph(Object(parsed.Graph))
	if len(problems) > 0 {
		result.Failure = "document_not_executable_graph"
		list := make([]any, 0, len(problems))
		for i := range problems {
			list = append(list, problems[i].Object())
		}
		result.Problems = list
		return result
	}
	result.Parsed = parsed
	return result
}

// postgresUnsupportedJSON reports why a document cannot be stored verbatim in a jsonb column. The
// check is on the raw bytes because Go's decoder has already lost the distinction: it accepts
// \u0000 (which PostgreSQL rejects), silently replaces unpaired surrogate escapes with U+FFFD
// (which PostgreSQL also rejects), and decodes numbers outside jsonb's numeric range (for example
// 1e-20000, which underflows to zero) without error. Writing such a document would fail the whole
// result transaction, so it must be classified before the write, beside the other document codes.
func postgresUnsupportedJSON(content []byte) (string, bool) {
	if !utf8.Valid(content) {
		return "invalid_utf8", true
	}
	inString := false
	for i := 0; i < len(content); i++ {
		if !inString {
			switch {
			case content[i] == '"':
				inString = true
			case content[i] == '-' || (content[i] >= '0' && content[i] <= '9'):
				width, fits := postgresNumberFits(content[i:])
				if !fits {
					return "numeric_out_of_range", true
				}
				i += width - 1
			}
			continue
		}
		switch content[i] {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(content) || content[i] != 'u' || i+4 >= len(content) {
				continue
			}
			code, ok := hex4(content[i+1 : i+5])
			if !ok {
				continue
			}
			i += 4
			switch {
			case code == 0:
				return "unsupported_unicode_escape", true
			case code >= 0xDC00 && code <= 0xDFFF:
				return "unsupported_unicode_escape", true
			case code >= 0xD800 && code <= 0xDBFF:
				// A high surrogate is representable only immediately followed by a low one; the
				// parser has already proved any escape has four hex digits.
				next := i + 1
				if next+5 >= len(content) || content[next] != '\\' || content[next+1] != 'u' {
					return "unsupported_unicode_escape", true
				}
				low, ok := hex4(content[next+2 : next+6])
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return "unsupported_unicode_escape", true
				}
				i = next + 5
			}
		}
	}
	return "", false
}

// jsonb stores numbers as PostgreSQL numeric, which keeps at most 131072 digits before the decimal
// point and 16383 after it. jsonb parses literals without a typmod, so these storage limits, not
// the smaller display precision, decide whether a document can be written verbatim.
const (
	postgresNumericMaxIntegerDigits = 131072
	postgresNumericMaxScale         = 16383
	// PostgreSQL bounds a numeric exponent at 2^30-1. Larger literals are rejected while parsing,
	// even when the entire mantissa is zero, so the magnitude check cannot rely on the mantissa.
	postgresNumericMaxExponent = 1<<30 - 1
)

// postgresNumberFits reports whether the JSON number at the start of content is representable as a
// PostgreSQL numeric, and how many bytes it spans. The document is already known to be valid JSON,
// so the scan only recovers the literal's digit layout and exponent. The scale follows the literal,
// not the mathematical value: trailing zeros count, so 10e-16384 overflows even though its value
// equals 1e-16383. The integer side is measured from the first significant digit, so leading zeros
// in the fraction (0.0001e131075) do not consume the magnitude budget.
func postgresNumberFits(content []byte) (int, bool) {
	i := 0
	if content[i] == '-' {
		i++
	}
	integerStart := i
	for i < len(content) && content[i] >= '0' && content[i] <= '9' {
		i++
	}
	integerDigits := i - integerStart
	fractionStart := i
	fractionDigits := 0
	if i < len(content) && content[i] == '.' {
		i++
		fractionStart = i
		for i < len(content) && content[i] >= '0' && content[i] <= '9' {
			i++
		}
		fractionDigits = i - fractionStart
	}
	exponent := int64(0)
	if i < len(content) && (content[i] == 'e' || content[i] == 'E') {
		i++
		negative := false
		if i < len(content) && (content[i] == '+' || content[i] == '-') {
			negative = content[i] == '-'
			i++
		}
		for i < len(content) && content[i] >= '0' && content[i] <= '9' {
			// Stop accumulating past the limit; any additional digit only makes the literal less
			// representable, and keeping the value bounded avoids overflow on long exponents.
			if exponent <= postgresNumericMaxExponent {
				exponent = exponent*10 + int64(content[i]-'0')
			}
			i++
		}
		if exponent > postgresNumericMaxExponent {
			return i, false
		}
		if negative {
			exponent = -exponent
		}
	}
	if int64(fractionDigits)-exponent > postgresNumericMaxScale {
		return i, false
	}
	first := -1
	for j := 0; j < integerDigits; j++ {
		if content[integerStart+j] != '0' {
			first = j
			break
		}
	}
	if first < 0 {
		for j := 0; j < fractionDigits; j++ {
			if content[fractionStart+j] != '0' {
				first = integerDigits + j
				break
			}
		}
	}
	if first >= 0 && int64(integerDigits-first)+exponent > postgresNumericMaxIntegerDigits {
		return i, false
	}
	return i, true
}

// hex4 decodes exactly four hexadecimal digits.
func hex4(digits []byte) (rune, bool) {
	var value rune
	for _, digit := range digits {
		switch {
		case digit >= '0' && digit <= '9':
			value = value*16 + rune(digit-'0')
		case digit >= 'a' && digit <= 'f':
			value = value*16 + rune(digit-'a'+10)
		case digit >= 'A' && digit <= 'F':
			value = value*16 + rune(digit-'A'+10)
		default:
			return 0, false
		}
	}
	return value, true
}

// assetFailureCode maps a fetch/extraction error to its bounded package code. Unexpected errors are
// unavailable rather than exposing transport or archive detail.
func assetFailureCode(err error) string {
	var packageFailure *workflowasset.Failure
	if errors.As(err, &packageFailure) {
		return packageFailure.Code
	}
	return workflowasset.FailureUnavailable
}

// finishWorkflowAssetImport writes per-document results and the package outcome in one transaction.
// A package failure produces no document rows, exactly as the decision separates package and
// document outcomes. The whole write is fenced on the claimed revision: a reinstall during the
// download leaves the row pending for the newer revision, and the stale attempt writes nothing.
// Writing v1 results here would occupy skip keys for paths the newer package should still decide.
func (s *Store) finishWorkflowAssetImport(ctx context.Context, job *workflowAssetJob, revision int64, results []workflowAssetParseResult, packageFailure string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		current := t.one(`SELECT requested_revision FROM workflow_asset_imports
			WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3`,
			job.SpaceID, job.Namespace, job.Identifier)
		if current == nil || current.N("requestedRevision") != revision {
			return nil
		}
		if packageFailure == "" {
			for i := range results {
				importWorkflowAssetDocument(t, job, &results[i])
			}
		}
		if packageFailure != "" {
			t.exec(`UPDATE workflow_asset_imports SET state='failed',failure_code=$4,lease_until=NULL,
				 completed_at=now(),version=version+1,updated_at=now()
				WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3 AND requested_revision=$5`,
				job.SpaceID, job.Namespace, job.Identifier, packageFailure, revision)
			return nil
		}
		t.exec(`UPDATE workflow_asset_imports SET state='completed',failure_code=NULL,lease_until=NULL,
			 completed_at=now(),version=version+1,updated_at=now()
			WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3 AND requested_revision=$4`,
			job.SpaceID, job.Namespace, job.Identifier, revision)
		return nil
	})
	if err != nil {
		return fmt.Errorf("record workflow asset import %s/%s for %s: %w", job.Namespace, job.Identifier, job.SpaceID, err)
	}
	return nil
}

// importWorkflowAssetDocument persists one result. A successfully imported path is the skip key
// and is never revisited; a failed path is refreshed so the next trigger retries it.
func importWorkflowAssetDocument(t *transaction, job *workflowAssetJob, result *workflowAssetParseResult) {
	existing := t.one(`SELECT outcome FROM workflow_asset_documents
		WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3 AND document_path=$4`,
		job.SpaceID, job.Namespace, job.Identifier, result.Document.Path)
	if existing != nil && existing.S("outcome") == "imported" {
		return
	}
	failureCode := result.Failure
	detail := any(nil)
	var workflowID, snapshotID string
	var snapshotVersion int64
	if failureCode == "" {
		name := result.Parsed.Name
		if t.one("SELECT id FROM workflows WHERE tenant_id=$1 AND name=$2 AND deleted_at IS NULL", job.TenantID, name) != nil {
			failureCode = "name_conflict"
		} else {
			w := insertWorkflow(t, job.TenantID, name, "", string(result.Document.Content))
			snapshot := insertSnapshotGraph(t, w, name, string(result.Document.Content))
			workflowID, snapshotID, snapshotVersion = w.S("id"), snapshot.S("id"), snapshot.N("version")
		}
	}
	switch {
	case failureCode == "name_conflict":
		detail = Object{}
	case len(result.Problems) > 0:
		detail = Object{"problems": result.Problems}
	case result.Reason != "":
		detail = Object{"reason": result.Reason}
	}
	t.exec(`INSERT INTO workflow_asset_documents(space_id,source_namespace,identifier,document_path,
			package_version,package_sha256,outcome,workflow_id,snapshot_id,snapshot_version,failure_code,failure_detail)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (space_id,source_namespace,identifier,document_path) DO UPDATE SET
		 package_version=EXCLUDED.package_version,
		 package_sha256=EXCLUDED.package_sha256,
		 outcome=EXCLUDED.outcome,
		 workflow_id=EXCLUDED.workflow_id,
		 snapshot_id=EXCLUDED.snapshot_id,
		 snapshot_version=EXCLUDED.snapshot_version,
		 failure_code=EXCLUDED.failure_code,
		 failure_detail=EXCLUDED.failure_detail,
		 version=workflow_asset_documents.version+1,
		 updated_at=now()`,
		job.SpaceID, job.Namespace, job.Identifier, result.Document.Path,
		job.PackageVersion, job.PackageSHA256,
		map[bool]string{true: "failed", false: "imported"}[failureCode != ""],
		nullable(workflowID), nullable(snapshotID), nullableInt(snapshotVersion),
		nullable(failureCode), jsonValue(detail))
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// spaceWorkflowAssetImportList is the independent read of import results. It is deliberately not
// nested in the plugin list: installation evidence and asset evidence are separate facts.
func spaceWorkflowAssetImportList(t *transaction, spaceID string) Object {
	items := t.list(`SELECT (source_namespace||'/'||identifier) AS plugin_id,
			source_namespace,identifier,package_version,package_sha256,state,failure_code,
			requested_revision,attempted_at,completed_at
		FROM workflow_asset_imports WHERE space_id=$1 ORDER BY source_namespace,identifier`, spaceID)
	for i := range items {
		items[i]["documents"] = t.list(`SELECT document_path,package_version,package_sha256,
				outcome,workflow_id,snapshot_version,failure_code,failure_detail,updated_at
			FROM workflow_asset_documents
			WHERE space_id=$1 AND source_namespace=$2 AND identifier=$3
			ORDER BY document_path`, spaceID, items[i].S("sourceNamespace"), items[i].S("identifier"))
	}
	return Object{"items": items}
}
