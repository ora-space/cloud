/**
 * Comparison operator labels for Condition nodes and Loop exit rules.
 *
 * The list is the consumer's vocabulary: the wire spellings are stable identifiers, so they live
 * next to the capability table and stay out of the feature's main bundle, which is held to a line
 * budget. `translations.ts` spreads both locales in.
 */
export const operatorTranslations = {
  'zh-CN': {
    'workflows.operator.equals': '等于',
    'workflows.operator.not-equals': '不等于',
    'workflows.operator.contains': '包含',
    'workflows.operator.not-contains': '不包含',
    'workflows.operator.starts-with': '开头是',
    'workflows.operator.ends-with': '结尾是',
    'workflows.operator.greater-than': '大于',
    'workflows.operator.greater-than-or-equal': '大于或等于',
    'workflows.operator.less-than': '小于',
    'workflows.operator.less-than-or-equal': '小于或等于',
    'workflows.operator.empty': '为空',
    'workflows.operator.not-empty': '不为空',
    'workflows.operator.exists': '已赋值',
    'workflows.operator.not-exists': '未赋值',
  },
  'en-US': {
    'workflows.operator.equals': 'equals',
    'workflows.operator.not-equals': 'does not equal',
    'workflows.operator.contains': 'contains',
    'workflows.operator.not-contains': 'does not contain',
    'workflows.operator.starts-with': 'starts with',
    'workflows.operator.ends-with': 'ends with',
    'workflows.operator.greater-than': 'is greater than',
    'workflows.operator.greater-than-or-equal': 'is greater than or equal to',
    'workflows.operator.less-than': 'is less than',
    'workflows.operator.less-than-or-equal': 'is less than or equal to',
    'workflows.operator.empty': 'is empty',
    'workflows.operator.not-empty': 'is not empty',
    'workflows.operator.exists': 'is assigned',
    'workflows.operator.not-exists': 'is not assigned',
  },
}
