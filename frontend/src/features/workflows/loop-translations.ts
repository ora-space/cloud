/**
 * Messages for the Loop container and its early-exit marker.
 *
 * They live beside the feature's main bundle for the same reason the history and launch-field
 * bundles do: the group is self-contained — the container's palette identity, the starter body the
 * factory builds, the inspector fields, and the `loopExit` marker — and keeping it here stops the
 * main bundle outgrowing the line budget that keeps its keys scannable. `translations.ts` spreads
 * both locales in, so the feature still ships one bundle.
 */
export const loopTranslations = {
  'zh-CN': {
    'workflows.node.loop.label': '循环',
    'workflows.node.loop.description': '重复执行直到满足条件',
    'workflows.node.loopExit.label': '早退',
    'workflows.node.loopExit.description': '循环体内的终端节点：被选中时提前结束循环',
    'workflows.loop.roundStart': '轮次开始',
    'workflows.loop.loopAgent': '循环 Agent',
    'workflows.loop.promptTemplate': '改进当前值并输出新值：{{#NODE_ID.value#}}',
    'workflows.loop.field.maxIterations': '最大轮次',
    'workflows.loop.field.maxIterationsBehavior': '轮次耗尽时',
    'workflows.loop.behavior.fail': '失败',
    'workflows.loop.behavior.succeed': '导出最后一轮并成功',
    'workflows.loop.field.initialValue': '初始值',
    'workflows.loop.field.variables': '携带变量',
    'workflows.loop.field.variableName': '变量名',
    'workflows.loop.field.variableType': '类型',
    'workflows.loop.field.variableInitialKind': '初值来源',
    'workflows.loop.initialKind.constant': '常量',
    'workflows.loop.initialKind.variable': '变量',
    'workflows.loop.field.variableInitial': '初值',
    'workflows.loop.field.variableFeedback': '反馈',
    'workflows.loop.addVariable': '添加变量',
    'workflows.loop.removeVariable': '删除变量',
    'workflows.loop.field.until': '结束条件',
    'workflows.loop.addUntilRule': '添加条件',
    'workflows.loop.addExitNode': '添加早退节点',
    'workflows.loop.removeUntilRule': '删除条件',
    'workflows.loop.field.outputs': '输出',
    'workflows.loop.field.outputName': '输出名',
    'workflows.loop.field.outputSelector': '取值',
    'workflows.loop.addOutput': '添加输出',
    'workflows.loop.removeOutput': '删除输出',
    'workflows.loop.selectorPlaceholder': '节点.变量，例如 writer.output',
    'workflows.loop.defaultBehavior':
      '每轮将 Agent 输出反馈为下一轮的 value；输出非空时结束，并导出为 result。',
    'workflows.loop.legacyUnsupported': '此旧循环节点缺少可执行配置，请删除后重新添加。',
  },
  'en-US': {
    'workflows.node.loop.label': 'Loop',
    'workflows.node.loop.description': 'Repeat until the exit condition is met',
    'workflows.node.loopExit.label': 'Early exit',
    'workflows.node.loopExit.description':
      'Terminal node inside a Loop body that ends the loop when selected',
    'workflows.loop.roundStart': 'Round start',
    'workflows.loop.loopAgent': 'Loop Agent',
    'workflows.loop.promptTemplate':
      'Improve the current value and output the new value: {{#NODE_ID.value#}}',
    'workflows.loop.field.maxIterations': 'Maximum rounds',
    'workflows.loop.field.maxIterationsBehavior': 'When rounds run out',
    'workflows.loop.behavior.fail': 'Fail',
    'workflows.loop.behavior.succeed': 'Publish the final round',
    'workflows.loop.field.initialValue': 'Initial value',
    'workflows.loop.field.variables': 'Carried variables',
    'workflows.loop.field.variableName': 'Variable name',
    'workflows.loop.field.variableType': 'Type',
    'workflows.loop.field.variableInitialKind': 'First value from',
    'workflows.loop.initialKind.constant': 'Constant',
    'workflows.loop.initialKind.variable': 'Variable',
    'workflows.loop.field.variableInitial': 'First value',
    'workflows.loop.field.variableFeedback': 'Feedback',
    'workflows.loop.addVariable': 'Add variable',
    'workflows.loop.removeVariable': 'Remove variable',
    'workflows.loop.field.until': 'Exit condition',
    'workflows.loop.addUntilRule': 'Add condition',
    'workflows.loop.addExitNode': 'Add early-exit node',
    'workflows.loop.removeUntilRule': 'Remove condition',
    'workflows.loop.field.outputs': 'Outputs',
    'workflows.loop.field.outputName': 'Output name',
    'workflows.loop.field.outputSelector': 'Value',
    'workflows.loop.addOutput': 'Add output',
    'workflows.loop.removeOutput': 'Remove output',
    'workflows.loop.selectorPlaceholder': 'node.variable, for example writer.output',
    'workflows.loop.defaultBehavior':
      'Each Agent output feeds the next round as value; a non-empty output stops the Loop and is exported as result.',
    'workflows.loop.legacyUnsupported':
      'This legacy Loop lacks executable configuration. Delete it and add a new Loop.',
  },
}
