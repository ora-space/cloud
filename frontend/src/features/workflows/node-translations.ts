/**
 * Palette identity of every workflow node kind.
 *
 * The node catalog stores these keys rather than resolved text, so a language switch never renames a
 * node the author already wrote. They live beside the feature's main bundle because the block is
 * self-contained and the main bundle is held to a line budget; `translations.ts` spreads both
 * locales in.
 */
export const nodeTranslations = {
  'zh-CN': {
    'workflows.condition.connectFromBranch': '从{{name}}的 {{branch}} 分支连接',
    'workflows.output.bindings': '输出绑定',
    'workflows.output.description':
      '为最终结果命名，并选择要返回的上游变量。未添加绑定时返回空结果。',
    'workflows.output.name': '输出名称',
    'workflows.output.namePlaceholder': '例如 summary',
    'workflows.output.source': '输出来源',
    'workflows.output.sourcePlaceholder': '选择变量',
    'workflows.output.add': '添加输出',
    'workflows.output.remove': '删除输出',
    // Node catalog: palette labels and descriptions for every executable kind.
    'workflows.node.start.label': '开始',
    'workflows.node.start.description': '定义工作流输入',
    'workflows.node.agent.label': 'Agent',
    'workflows.node.agent.description': '交给模型自主执行',
    'workflows.node.condition.label': '条件分支',
    'workflows.node.condition.description': '根据规则选择路径',
    'workflows.node.tool.label': '工具',
    'workflows.node.tool.description': '调用终端或插件',
    'workflows.node.junction.label': '汇合',
    'workflows.node.junction.description': '等待多个执行分支完成',
    'workflows.node.human.label': '人工确认',
    'workflows.node.human.description': '等待人工决策后继续',
    'workflows.node.loop.label': '循环',
    'workflows.node.loop.description': '重复执行直到满足条件',
    'workflows.node.iteration.label': '迭代',
    'workflows.node.iteration.description': '对数组逐项执行区域内节点',
    'workflows.node.subflow.label': '子流程',
    'workflows.node.subflow.description': '封装复杂业务步骤',
    'workflows.node.output.label': '输出',
    'workflows.node.output.description': '返回最终结果',
  },
  'en-US': {
    'workflows.condition.connectFromBranch': 'Connect from {{branch}} of {{name}}',
    'workflows.output.bindings': 'Output bindings',
    'workflows.output.description':
      'Name each result and choose the upstream variable to return. No bindings produce an empty result.',
    'workflows.output.name': 'Output name',
    'workflows.output.namePlaceholder': 'For example, summary',
    'workflows.output.source': 'Output source',
    'workflows.output.sourcePlaceholder': 'Choose a variable',
    'workflows.output.add': 'Add output',
    'workflows.output.remove': 'Remove output',
    'workflows.node.start.label': 'Start',
    'workflows.node.start.description': 'Define workflow inputs',
    'workflows.node.agent.label': 'Agent',
    'workflows.node.agent.description': 'Delegate autonomous work to a model',
    'workflows.node.condition.label': 'Condition',
    'workflows.node.condition.description': 'Route execution based on rules',
    'workflows.node.tool.label': 'Tool',
    'workflows.node.tool.description': 'Call a terminal or plugin',
    'workflows.node.junction.label': 'Merge',
    'workflows.node.junction.description': 'Wait for multiple branches to complete',
    'workflows.node.human.label': 'Human confirmation',
    'workflows.node.human.description': 'Pause for a human decision',
    'workflows.node.loop.label': 'Loop',
    'workflows.node.loop.description': 'Repeat until the exit condition is met',
    'workflows.node.iteration.label': 'Iteration',
    'workflows.node.iteration.description': 'Run the region once per array element',
    'workflows.node.subflow.label': 'Subflow',
    'workflows.node.subflow.description': 'Encapsulate a complex business step',
    'workflows.node.output.label': 'Output',
    'workflows.node.output.description': 'Return the final result',
  },
}
