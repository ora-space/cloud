import type { TranslationBundle } from '@/i18n/resource-bundle'

/** Publish admission guidance shared by the fault summary and the problem list. */
export const publishErrorTranslations = {
  'zh-CN': {
    'workflows.errors.admission.scope': '只能选择当前作用域内的传递前驱变量。',
    'workflows.errors.admission.selector': '变量未声明或在此作用域不可见，请重新选择。',
    'workflows.errors.admission.limit': '轮次上限必须是 1 到 100 的整数。',
    'workflows.errors.admission.config': '缺少节点配置，请补全后发布。',
    'workflows.errors.admission.entry': '循环体必须恰好包含一个开始节点。',
    'workflows.errors.admission.region': '请检查成员归属及区域内部可达性，容器不能为空。',
    'workflows.errors.admission.edge': '连线不能跨越容器边界。',
    'workflows.errors.admission.exit': '早退节点必须位于循环体内且不能有出边。',
    'workflows.errors.admission.nested': '区域内不支持嵌套容器或输出节点。',
    'workflows.errors.admission.variable': '变量名必须合法且唯一，类型必须受支持。',
    'workflows.errors.admission.initial': '初值必须匹配变量类型；变量初值只能引用外层可见根。',
    'workflows.errors.admission.feedback': '请选择本轮可见的反馈变量。',
    'workflows.errors.admission.until': '请补全终止条件的逻辑、比较运算和变量。',
    'workflows.errors.admission.output': '输出名必须合法且唯一，并绑定已声明的变量。',
    'workflows.errors.admission.collect': '收集目标必须是当前区域成员声明的输出根。',
    'workflows.errors.admission.strategy': '请选择受支持的失败或耗尽策略。',
    'workflows.errors.admission.candidates': '候选必须是非空、无重复的合法变量选择器列表。',
    'workflows.inspector.field.structuredOutputHint':
      'Cloud 当前不支持结构化输出；开启后无法发布。关闭此设置可使用普通文本输出。',
    'workflows.errors.graphInvalid':
      '工作流配置未通过发布检查，请检查节点、连线和变量配置后重新发布。',
    'workflows.errors.graphProblems': '工作流配置未通过发布检查，请修正以下问题后重新发布。',
    'workflows.errors.problemNode': '节点 {{node}}',
    'workflows.errors.problemCheck': '请检查此处的节点、连线或变量配置。',
    'workflows.errors.structuredOutputUnsupported':
      'Cloud 当前不支持结构化输出，请关闭该节点的「结构化输出」后重新发布。',
    'workflows.errors.configUnsupported': 'Cloud 当前不支持此配置，请关闭或移除后重新发布。',
    'workflows.errors.agentSchemaVersionUnsupported':
      'Cloud 只支持 Agent 配置版本 3，请使用兼容的工作流文档。',
    'workflows.errors.nodeKindUnsupported': 'Cloud 当前不支持此节点类型，请替换或移除该节点。',
  },
  'en-US': {
    'workflows.errors.admission.scope': 'Select a transitive predecessor in this scope.',
    'workflows.errors.admission.selector':
      'The variable is undeclared or outside this scope. Select another variable.',
    'workflows.errors.admission.limit': 'The round limit must be an integer from 1 to 100.',
    'workflows.errors.admission.config':
      'Complete the missing node configuration before publishing.',
    'workflows.errors.admission.entry': 'A loop body must contain exactly one Start node.',
    'workflows.errors.admission.region':
      'Check region membership and reachability; the region cannot be empty.',
    'workflows.errors.admission.edge': 'Connections cannot cross container boundaries.',
    'workflows.errors.admission.exit':
      'A loop exit must be inside a loop body and have no outgoing connections.',
    'workflows.errors.admission.nested':
      'Nested containers and Output nodes are not supported in a region.',
    'workflows.errors.admission.variable':
      'Variable names must be valid and unique, with supported types.',
    'workflows.errors.admission.initial':
      'Initial values must match the declared type; initial selectors must reference visible outer roots.',
    'workflows.errors.admission.feedback':
      'Select a feedback variable visible in the current round.',
    'workflows.errors.admission.until':
      'Complete the stop condition logic, comparisons, and variables.',
    'workflows.errors.admission.output':
      'Output names must be valid and unique and bind declared variables.',
    'workflows.errors.admission.collect':
      'Collect a declared output root from a member of this region.',
    'workflows.errors.admission.strategy': 'Choose a supported failure or exhaustion policy.',
    'workflows.errors.admission.candidates':
      'Candidates must be a nonempty list of valid, distinct variable selectors.',
    'workflows.inspector.field.structuredOutputHint':
      'Cloud does not support structured output yet; enabling it prevents publishing. Turn it off to use plain text output.',
    'workflows.errors.graphInvalid':
      'The workflow cannot be published. Check its nodes, connections and variables, then publish again.',
    'workflows.errors.graphProblems':
      'The workflow cannot be published. Fix the following problems and publish again.',
    'workflows.errors.problemNode': 'Node {{node}}',
    'workflows.errors.problemCheck': 'Check the node, connection or variable configuration here.',
    'workflows.errors.structuredOutputUnsupported':
      'Cloud does not support structured output yet. Turn off Structured output on this node and publish again.',
    'workflows.errors.configUnsupported':
      'Cloud does not support this setting yet. Turn it off or remove it, then publish again.',
    'workflows.errors.agentSchemaVersionUnsupported':
      'Cloud only supports Agent configuration version 3. Use a compatible workflow document.',
    'workflows.errors.nodeKindUnsupported':
      'Cloud does not support this node type yet. Replace or remove the node.',
  },
} satisfies TranslationBundle

/** Maps stable admission codes to localized, actionable guidance. */
export const workflowAdmissionKeys: Readonly<
  Record<string, keyof (typeof publishErrorTranslations)['en-US']>
> = {
  aggregator_selector_not_upstream: 'workflows.errors.admission.scope',
  iteration_iterator_not_upstream: 'workflows.errors.admission.scope',
  aggregator_selector_undeclared: 'workflows.errors.admission.selector',
  loop_selector_undeclared: 'workflows.errors.admission.selector',
  iteration_iterator_undeclared: 'workflows.errors.admission.selector',
  output_selector_undeclared: 'workflows.errors.admission.selector',
  loop_max_iterations_invalid: 'workflows.errors.admission.limit',
  iteration_max_iterations_invalid: 'workflows.errors.admission.limit',
  loop_config_missing: 'workflows.errors.admission.config',
  iteration_config_missing: 'workflows.errors.admission.config',
  aggregator_config_missing: 'workflows.errors.admission.config',
  loop_body_start_count: 'workflows.errors.admission.entry',
  loop_member_container_mismatch: 'workflows.errors.admission.region',
  loop_body_unreachable: 'workflows.errors.admission.region',
  iteration_body_empty: 'workflows.errors.admission.region',
  iteration_body_unreachable: 'workflows.errors.admission.region',
  loop_cross_container_edge: 'workflows.errors.admission.edge',
  loop_exit_has_outgoing_edge: 'workflows.errors.admission.exit',
  loop_exit_outside_body: 'workflows.errors.admission.exit',
  loop_nested_composite_unsupported: 'workflows.errors.admission.nested',
  loop_body_output_unsupported: 'workflows.errors.admission.nested',
  loop_variable_invalid: 'workflows.errors.admission.variable',
  loop_initial_invalid: 'workflows.errors.admission.initial',
  loop_feedback_invalid: 'workflows.errors.admission.feedback',
  loop_until_invalid: 'workflows.errors.admission.until',
  loop_output_invalid: 'workflows.errors.admission.output',
  output_binding_invalid: 'workflows.errors.admission.output',
  output_name_duplicate: 'workflows.errors.admission.output',
  output_name_invalid: 'workflows.errors.admission.output',
  output_selector_invalid: 'workflows.errors.admission.output',
  output_bindings_invalid: 'workflows.errors.admission.output',
  output_bindings_not_a_list: 'workflows.errors.admission.output',
  output_selector_unsupported: 'workflows.errors.admission.output',
  iteration_collect_outside_region: 'workflows.errors.admission.collect',
  iteration_collect_unsupported: 'workflows.errors.admission.collect',
  iteration_error_strategy_unknown: 'workflows.errors.admission.strategy',
  loop_max_iterations_behavior_invalid: 'workflows.errors.admission.strategy',
  aggregator_selector_duplicate: 'workflows.errors.admission.candidates',
  aggregator_selector_unsupported: 'workflows.errors.admission.candidates',
  aggregator_variables_empty: 'workflows.errors.admission.candidates',
  aggregator_variables_not_a_list: 'workflows.errors.admission.candidates',
  iteration_iterator_unsupported: 'workflows.errors.admission.candidates',
}
