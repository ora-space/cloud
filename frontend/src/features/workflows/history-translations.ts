/**
 * Messages for the editor's session history.
 *
 * They live beside the feature's main bundle rather than inside it because the group is
 * self-contained — the history panel, the undo/redo controls, and the labels each recorded edit is
 * listed under — and the main bundle is held to a line budget that keeps a reader able to find a key
 * by scanning it. `translations.ts` spreads these in, so the feature still ships one bundle.
 *
 * `event*` keys are named after the history event kinds the editor records, so a new event adds its
 * label here rather than somewhere in the middle of the main bundle.
 */
export const historyTranslations = {
  'zh-CN': {
    'workflows.history.undo': '撤销',
    'workflows.history.redo': '重做',
    'workflows.history.history': '编辑历史',
    'workflows.history.historyHint': '查看本次编辑的操作记录（撤销/重做），与运行记录、版本无关',
    'workflows.history.undoHint': '撤销上一步操作（Ctrl+Z）',
    'workflows.history.redoHint': '重做被撤销的操作（Ctrl+Shift+Z）',
    'workflows.history.current': '当前状态',
    'workflows.history.stepsBack': '回溯 {{count}} 步',
    'workflows.history.stepsForward': '前移 {{count}} 步',
    'workflows.history.clear': '清空历史',
    'workflows.history.empty': '还没有可回退的操作。',
    'workflows.history.sessionStart': '会话开始',
    'workflows.history.eventNodeAdd': '添加节点',
    'workflows.history.eventNodeDelete': '删除节点',
    'workflows.history.eventEdgeDelete': '删除连线',
    'workflows.history.eventEdgeConnect': '建立连线',
    'workflows.history.eventNodeMove': '移动节点',
    'workflows.history.eventIterationResize': '调整区域大小',
    'workflows.history.eventOrganize': '自动整理',
    'workflows.history.eventNodeEdit': '编辑节点',
    'workflows.history.eventVariables': '编辑全局变量',
    'workflows.history.eventLaunchFields': '编辑 @ 表单字段',
    'workflows.history.unknownNode': '未命名节点',
  },
  'en-US': {
    'workflows.history.undo': 'Undo',
    'workflows.history.redo': 'Redo',
    'workflows.history.history': 'Edit history',
    'workflows.history.historyHint':
      'Review the edits made in this session (undo/redo), not runs or versions',
    'workflows.history.undoHint': 'Undo the last edit (Ctrl+Z)',
    'workflows.history.redoHint': 'Redo the last undone edit (Ctrl+Shift+Z)',
    'workflows.history.current': 'Current state',
    'workflows.history.stepsBack': '{{count}} steps back',
    'workflows.history.stepsForward': '{{count}} steps forward',
    'workflows.history.clear': 'Clear history',
    'workflows.history.empty': 'Nothing to undo yet.',
    'workflows.history.sessionStart': 'Session start',
    'workflows.history.eventNodeAdd': 'Add node',
    'workflows.history.eventNodeDelete': 'Delete node',
    'workflows.history.eventEdgeDelete': 'Delete connection',
    'workflows.history.eventEdgeConnect': 'Connect nodes',
    'workflows.history.eventNodeMove': 'Move node',
    'workflows.history.eventIterationResize': 'Resize region',
    'workflows.history.eventOrganize': 'Auto arrange',
    'workflows.history.eventNodeEdit': 'Edit node',
    'workflows.history.eventVariables': 'Edit global variables',
    'workflows.history.eventLaunchFields': 'Edit @ form fields',
    'workflows.history.unknownNode': 'Unnamed node',
  },
}
