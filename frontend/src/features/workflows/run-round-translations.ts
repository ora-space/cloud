/**
 * Messages for the run viewer's round projection of Loop and Iteration containers.
 *
 * They live beside the feature's main bundle like the loop and aggregator groups: the round
 * summary on a container card, the round list and member table in the inspector, and the names
 * of round outcomes form one self-contained group. `translations.ts` spreads both locales in.
 */
export const runRoundTranslations = {
  'zh-CN': {
    'workflows.run.rounds.loopSummary': '第 {{round}} / {{max}} 轮',
    'workflows.run.rounds.iterationSummary': '{{settled}} / {{total}} 项',
    'workflows.run.rounds.iterationFailed': '失败 {{count}}',
    'workflows.run.rounds.title': '轮次',
    'workflows.run.rounds.empty': '没有执行任何一轮。',
    'workflows.run.rounds.loopRound': '第 {{index}} 轮',
    'workflows.run.rounds.iterationRound': '第 {{index}} 项',
    'workflows.run.rounds.members': '本轮成员',
    'workflows.run.rounds.item': '本项输入',
    'workflows.run.rounds.collected': '本项收集到',
    'workflows.run.rounds.notRun': '本轮未执行',
    'workflows.run.rounds.viewing': '查看轮次',
    'workflows.run.rounds.reason': '原因：{{reason}}',
    'workflows.run.rounds.outcome.running': '进行中',
    'workflows.run.rounds.outcome.failed': '失败',
    'workflows.run.rounds.outcome.cancelled': '已取消',
    'workflows.run.rounds.outcome.continued': '进入下一轮',
    'workflows.run.rounds.outcome.conditionMet': '条件满足',
    'workflows.run.rounds.outcome.loopExit': '早退',
    'workflows.run.rounds.outcome.limitReached': '轮次耗尽',
    'workflows.run.rounds.outcome.completed': '已完成',
    'workflows.run.rounds.outcome.succeeded': '成功',
    'workflows.run.rounds.outcome.absorbed': '失败（已继续）',
  },
  'en-US': {
    'workflows.run.rounds.loopSummary': 'Round {{round}} / {{max}}',
    'workflows.run.rounds.iterationSummary': '{{settled}} / {{total}} items',
    'workflows.run.rounds.iterationFailed': '{{count}} failed',
    'workflows.run.rounds.title': 'Rounds',
    'workflows.run.rounds.empty': 'No round ran.',
    'workflows.run.rounds.loopRound': 'Round {{index}}',
    'workflows.run.rounds.iterationRound': 'Item {{index}}',
    'workflows.run.rounds.members': 'Members this round',
    'workflows.run.rounds.item': 'Item input',
    'workflows.run.rounds.collected': 'Collected from this item',
    'workflows.run.rounds.notRun': 'Did not run this round',
    'workflows.run.rounds.viewing': 'Viewing round',
    'workflows.run.rounds.reason': 'Reason: {{reason}}',
    'workflows.run.rounds.outcome.running': 'In progress',
    'workflows.run.rounds.outcome.failed': 'Failed',
    'workflows.run.rounds.outcome.cancelled': 'Cancelled',
    'workflows.run.rounds.outcome.continued': 'Continued',
    'workflows.run.rounds.outcome.conditionMet': 'Condition met',
    'workflows.run.rounds.outcome.loopExit': 'Exited early',
    'workflows.run.rounds.outcome.limitReached': 'Rounds used up',
    'workflows.run.rounds.outcome.completed': 'Completed',
    'workflows.run.rounds.outcome.succeeded': 'Succeeded',
    'workflows.run.rounds.outcome.absorbed': 'Failed (continued)',
  },
}
