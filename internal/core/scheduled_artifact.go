package core

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Cloud 定时运行的产物交付（specs decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md，
// approved 的 D3/D4/D5）。
//
// 声明由 Node 执行端在会话终态里上报，随后不可变（留在 node_executions.result）。Cloud 在这里校验上限与
// 路径规则、为每个被接受的条目生成对象键、冻结不可续期的交付截止，并把被拒候选登记下来。产物本身在
// Workspace 释放前经上传授权写入对象存储；登记只发生在 Cloud 用 HEAD 复核 size/SHA-256 之后，因此
// scheduled_run_artifacts 里的每一行都是已核对的对象，而不是声明。
//
// 产物失败、超时或迟到都不改变 IssueRun 状态、Revision 交付与回收语义：delivering 等产物收敛或窗口到期，
// 然后照常释放。

const (
	// scheduledArtifactDeadline 是 ADR D3 冻结的产物上传窗口：登记时刻 + 10 分钟，以运行身份唯一且不可续期。
	scheduledArtifactDeadline = 10 * time.Minute
	// 单产物 10 MiB、单运行 64 个、单运行总计 64 MiB：Node 与 Cloud 两侧都校验，以云端上限为准。
	maxScheduledArtifactBytes      = 10 * 1024 * 1024
	maxScheduledArtifactItems      = 64
	maxScheduledArtifactTotalBytes = 64 * 1024 * 1024
	// 展示用相对路径的上限；它不参与对象寻址，只约束存储与展示。
	maxScheduledArtifactNameBytes = 1024
)

// scheduledArtifactKinds 是 ADR D3 冻结的类型白名单闭集。
var scheduledArtifactKinds = map[string]bool{
	"markdown": true, "json": true, "text": true, "log": true, "xml": true, "html": true,
}

// scheduledArtifactRejectionReasons 是执行端可以上报的拒绝原因闭集。Cloud 自己判定的失败
// （upload_failed、verification_failed）不使用这组词，它们只出现在 Cloud 结算时写入的行里。
var scheduledArtifactRejectionReasons = map[string]bool{
	"unsupported_type": true, "path_unsafe": true, "symlink": true, "hard_link": true,
	"too_large": true, "too_many": true, "total_too_large": true, "read_failed": true,
	"candidate_limit": true,
}

// scheduledArtifact 是校验通过的一条声明，带有 Cloud 生成的对象键。
type scheduledArtifact struct {
	Index       int
	Key         string
	DisplayName string
	Kind        string
	Size        int64
	Sha256      string
}

// scheduledArtifactRejection 是一条被拒候选：执行端拒绝的，或 Cloud 结算时判定失败的。
type scheduledArtifactRejection struct {
	DisplayName string
	Reason      string
}

// scheduledArtifactDeclaration 是一次声明在 Cloud 侧的权威视图。
type scheduledArtifactDeclaration struct {
	Artifacts         []scheduledArtifact
	Rejections        []scheduledArtifactRejection
	OmittedRejections int64
	WalkTruncated     bool
}

// scheduledArtifactKey 生成一个条目的对象键。键只取决于运行身份与声明序号，绝不使用用户文件名，所以
// 同一运行的同一序号在重派、重启与重放里始终指向同一个对象。
func scheduledArtifactKey(tenantID, runID string, index int) string {
	return fmt.Sprintf("scheduled-runs/%s/%s/%d", tenantID, runID, index)
}

// parseScheduledArtifactManifest 校验上报的清单并交付 Cloud 自己的对象键。
//
// 这里拒绝的是任何会破坏存储或读取语义的声明：超过三项上限、序号重复或越界、类型不在白名单、大小或摘要
// 不合规、展示路径不是安全相对路径。执行端本应已经在采集时拦下这些，所以一旦出现就是矛盾的上报，
// 拒绝整份声明而不是挑选一部分——半个清单会让运行看起来有产物而事实不全。
func parseScheduledArtifactManifest(tenantID, runID string, manifest Object) (scheduledArtifactDeclaration, error) {
	omitted := manifest.N("omittedRejections")
	if omitted < 0 || omitted > math.MaxUint32 {
		return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: invalid omitted rejection count")
	}
	declaration := scheduledArtifactDeclaration{OmittedRejections: omitted, WalkTruncated: manifest.B("walkTruncated")}
	seen := map[string]bool{}
	indices := map[int64]bool{}
	var total int64
	entries := objectRows(manifest["items"])
	if len(entries) > maxScheduledArtifactItems {
		return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared %d artifacts, over the %d limit", runID, len(entries), maxScheduledArtifactItems)
	}
	for _, entry := range entries {
		index := entry.N("index")
		if index < 0 || index >= maxScheduledArtifactItems {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared index %d outside 0..%d", runID, index, maxScheduledArtifactItems-1)
		}
		if indices[index] {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared index %d twice", runID, index)
		}
		indices[index] = true
		if seen[entry.S("displayName")] {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared %q twice", runID, entry.S("displayName"))
		}
		seen[entry.S("displayName")] = true
		name := entry.S("displayName")
		if !scheduledArtifactPath(name) {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared unsafe path %q", runID, name)
		}
		kind := entry.S("kind")
		if !scheduledArtifactKinds[kind] {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared kind %q outside the whitelist", runID, kind)
		}
		size := entry.N("sizeBytes")
		if size < 0 || size > maxScheduledArtifactBytes {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared %d bytes, outside 0..%d", runID, size, maxScheduledArtifactBytes)
		}
		digest := entry.S("sha256")
		if !lowerHexDigest(digest) {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared an invalid SHA-256", runID)
		}
		total += size
		if total > maxScheduledArtifactTotalBytes {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s declared more than %d bytes", runID, maxScheduledArtifactTotalBytes)
		}
		declaration.Artifacts = append(declaration.Artifacts, scheduledArtifact{
			Index:       int(index),
			Key:         scheduledArtifactKey(tenantID, runID, int(index)),
			DisplayName: name,
			Kind:        kind,
			Size:        size,
			Sha256:      digest,
		})
	}
	rejections := objectRows(manifest["rejections"])
	if len(rejections) > maxScheduledArtifactItems {
		return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: too many rejection details")
	}
	for _, entry := range rejections {
		name, reason := entry.S("displayName"), entry.S("reason")
		// A rejected path can be unsafe by definition. It is bounded display text, never a key or
		// a filesystem path Cloud opens, so retain it instead of rejecting the session terminal.
		if name == "" || len(name) > maxScheduledArtifactNameBytes || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || !scheduledArtifactRejectionReasons[reason] {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s reported an invalid rejection (%q, %q)", runID, name, reason)
		}
		if seen[name] {
			return scheduledArtifactDeclaration{}, fmt.Errorf("artifactManifest: run %s both declared and rejected %q", runID, name)
		}
		declaration.Rejections = append(declaration.Rejections, scheduledArtifactRejection{DisplayName: name, Reason: reason})
	}
	return declaration, nil
}

// scheduledArtifactPath reports whether one declared name is a safe forward-slash relative path.
//
// The name only ever reaches a human reader, but a value that escapes its directory or carries a
// Windows alternate data stream would still be misread by whatever opens it next, so the same shape
// the executor collects is enforced here again.
func scheduledArtifactPath(name string) bool {
	if name == "" || len(name) > maxScheduledArtifactNameBytes || strings.HasPrefix(name, "/") || strings.ContainsAny(name, ":\\") {
		return false
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// lowerHexDigest accepts the digest spelling Cloud verifies uploads against.
func lowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// objectRows reads a JSON array of objects out of a decoded durable object.
func objectRows(v any) []Object {
	switch typed := v.(type) {
	case []Object:
		return typed
	case []any:
		rows := make([]Object, 0, len(typed))
		for _, entry := range typed {
			switch object := entry.(type) {
			case Object:
				rows = append(rows, object)
			case map[string]any:
				rows = append(rows, Object(object))
			}
		}
		return rows
	default:
		return nil
	}
}
