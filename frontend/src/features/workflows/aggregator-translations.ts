/**
 * Messages for the variable aggregator node.
 *
 * They live beside the feature's main bundle for the same reason the history and launch-field
 * bundles do: the group is self-contained — the node's palette identity and the ordered candidate
 * list — and the main bundle is held to a line budget. `translations.ts` spreads both locales in.
 */
export const aggregatorTranslations = {
  'zh-CN': {
    'workflows.node.aggregator.label': '变量聚合',
    'workflows.node.aggregator.description': '按顺序取第一个已赋值的候选变量',
    'workflows.aggregator.field.variables': '候选变量',
    'workflows.aggregator.field.candidate': '候选',
    'workflows.aggregator.addCandidate': '添加候选',
    'workflows.aggregator.removeCandidate': '删除候选',
    'workflows.aggregator.moveCandidateUp': '上移候选',
    'workflows.aggregator.moveCandidateDown': '下移候选',
    'workflows.aggregator.selectorPlaceholder': '节点.变量，例如 writer.output',
  },
  'en-US': {
    'workflows.node.aggregator.label': 'Variable aggregator',
    'workflows.node.aggregator.description': 'Pass through the first assigned candidate, in order',
    'workflows.aggregator.field.variables': 'Candidates',
    'workflows.aggregator.field.candidate': 'Candidate',
    'workflows.aggregator.addCandidate': 'Add candidate',
    'workflows.aggregator.removeCandidate': 'Remove candidate',
    'workflows.aggregator.moveCandidateUp': 'Move candidate up',
    'workflows.aggregator.moveCandidateDown': 'Move candidate down',
    'workflows.aggregator.selectorPlaceholder': 'node.variable, for example writer.output',
  },
}
