package core

import (
	"fmt"
	"unicode/utf8"
)

// Cloud 定时运行的业务 verdict 落点（specs decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md，
// approved 的 D1/D2）。verdict 与执行层状态相互独立：completed 的运行可以携带 fail，两者都不从对方推导。
//
// 可信写入者是 Node 执行端：它在会话收尾时从最终 Assistant 轮解析哨兵文本，把结构化值放进
// AgentSessionEnded.scheduled_result 上报。Cloud 在这里只做最后一道校验（枚举、detail 长度、运行身份），
// 不读取也不解析 Thread、消息或工具业务字段。

// MaxScheduledRunVerdictDetailRunes 是 ADR D1 冻结的 detail 上限，按 Unicode 字符计。控制面与核心共用同一个
// 定义，避免两处各自写死一个数字后允许的值不一致。
const MaxScheduledRunVerdictDetailRunes = 2000

// scheduledRunVerdicts 是 Cloud 与 local 共用的 verdict 闭集，DTO 与列都使用同一组稳定词。
var scheduledRunVerdicts = map[string]bool{
	"pass":             true,
	"warn":             true,
	"fail":             true,
	"blocked":          true,
	"idle":             true,
	"range_unresolved": true,
}

// scheduledRunResultIsReportable reports whether a session end reason may carry business facts.
// Only an end Cloud itself asked for (`user_ended` / `idle_timeout`) completed the turn whose
// conclusion the verdict describes; a cancelled, failed or interrupted session has none.
func scheduledRunResultIsReportable(reason string) bool {
	return reason == "user_ended" || reason == "idle_timeout"
}

// recordScheduledRunResult writes the business verdict this session reported, at most once.
//
// It runs inside the caller's terminal-event takeover transaction, so the verdict commits and rolls
// back with the Thread terminal state and the delivery release. A run that is not a scheduled
// occurrence has no authoritative task identity to freeze a result against and must not have
// reported one, so a scheduled_result on such a run is refused rather than dropped: it would mean the
// executor was told to report business facts Cloud cannot attribute.
//
// A replay of the same terminal event finds the row already written and leaves it untouched; a later
// event or any other caller can neither overwrite nor clear it (`ON CONFLICT DO NOTHING`). No row is
// written when the executor reported nothing, which the reader projects as "not reported".
func recordScheduledRunResult(t *transaction, runID string, ended Object) error {
	result := ended.O("scheduledResult")
	if len(result) == 0 {
		return nil
	}
	if !scheduledRunResultIsReportable(ended.S("reason")) {
		return nil
	}
	verdict := result.S("verdict")
	if !scheduledRunVerdicts[verdict] {
		return fmt.Errorf("sessionEnded: run %s reported verdict %q, outside the closed set", runID, verdict)
	}
	detail := result.S("verdictDetail")
	if utf8.RuneCountInString(detail) > MaxScheduledRunVerdictDetailRunes {
		return fmt.Errorf("sessionEnded: run %s reported a verdict detail over %d characters", runID, MaxScheduledRunVerdictDetailRunes)
	}
	occurrence := t.one(`
		SELECT id, tenant_id, task_id, scheduled_at FROM scheduled_task_occurrences
		WHERE run_id=$1 AND outcome='admitted'`, runID)
	if occurrence == nil {
		return fmt.Errorf("sessionEnded: run %s reported a scheduled verdict but is not an admitted scheduled run", runID)
	}
	var stored any
	if detail != "" {
		stored = detail
	}
	t.exec(`
		INSERT INTO scheduled_run_results
			(issue_run_id, tenant_id, task_id, occurrence_id, scheduled_at, verdict, verdict_detail, verdict_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (issue_run_id) DO NOTHING`,
		runID, occurrence.S("tenantId"), occurrence.S("taskId"), occurrence.S("id"), occurrence.S("scheduledAt"), verdict, stored)
	return nil
}
