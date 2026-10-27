/**
 * Messages for the workflow detail page's panels: the tab labels, the run history and the
 * published versions.
 *
 * They live beside the feature's main bundle rather than inside it for the reason the editor
 * history has its own file: the main bundle is held to a line budget that keeps a reader able to
 * find a key by scanning it, and these three groups are one surface — what a member reads after
 * choosing a tab, not while editing the canvas. `translations.ts` spreads them in, so the feature
 * still ships one bundle.
 */
export const runAndVersionTranslations = {
  'zh-CN': {
    'workflows.version.publish': '发布',
    'workflows.version.publishHint': '把当前画布冻结为一个不可变版本',
    'workflows.version.history': '版本历史',
    'workflows.version.historyHint': '查看已发布的版本并回滚',
    'workflows.version.publishTitle': '发布版本',
    'workflows.version.publishDescription':
      '发布会把当前文档冻结为一个不可变版本；之后可以随时回滚到这里。',
    'workflows.version.versionName': '版本名称（可选）',
    'workflows.version.versionPlaceholder': '例如：v1.0 发布说明',
    'workflows.version.publishedAt': '发布于 {{time}}',
    'workflows.version.historyEmpty': '还没有已发布的版本。发布一个以随时回滚。',
    'workflows.version.historySearch': '搜索版本名称或编号',
    'workflows.version.restore': '回滚',
    'workflows.version.restoreTitle': '回滚到版本 {{version}}？',
    'workflows.version.restoreDescription':
      '回滚会用该版本冻结的图替换当前画布，未保存的修改会丢失。',
    'workflows.version.rollbackNamed': '回滚到版本 {{version}}',
    'workflows.tabs.label': '工作流的分区',
    'workflows.tabs.editor': '编辑',
    'workflows.tabs.versions': '版本',
    'workflows.draftNavigation.title': '草稿保存失败',
    'workflows.draftNavigation.description':
      '修改尚未保存。你可以留在编辑器继续处理，或放弃本次未保存的修改并离开。',
    'workflows.draftNavigation.stay': '留在编辑器',
    'workflows.draftNavigation.leave': '放弃修改并离开',
    'workflows.runs.loading': '正在加载运行记录…',
    'workflows.runs.emptyTitle': '还没有运行记录',
    'workflows.runs.emptyDescription':
      '发布版本后在「编辑」里点「运行」启动一次；每次运行都会列在这里。',
    'workflows.runs.failedTitle': '运行记录读取失败',
    'workflows.runs.failedDescription':
      '这次读取没有成功，可以重试。这不代表这个工作流没有运行过。',
    'workflows.runs.forbiddenTitle': '没有查看运行记录的权限',
    'workflows.runs.forbiddenDescription': '你在这个空间里的角色不能读取这个工作流的运行历史。',
    'workflows.runs.retry': '重试',
    'workflows.runs.loadMore': '加载更早的运行',
    'workflows.runs.loadingMore': '正在加载…',
    'workflows.runs.loadMoreFailed': '更早的运行读取失败。',
    'workflows.runs.endOfHistory': '已经到最早的一次运行。',
    'workflows.runs.open': '打开运行 {{name}}',
    'workflows.runs.version': '版本 {{id}}',
    'workflows.runs.started': '开始 {{time}}',
    'workflows.runs.finished': '结束 {{time}}',
    'workflows.runs.notStarted': '未开始（创建于 {{time}}）',
    'workflows.runs.notFinished': '尚未结束',
  },
  'en-US': {
    'workflows.version.publish': 'Publish',
    'workflows.version.publishHint': 'Freeze the current canvas as an immutable version',
    'workflows.version.history': 'Version history',
    'workflows.version.historyHint': 'View published versions and roll back',
    'workflows.version.publishTitle': 'Publish version',
    'workflows.version.publishDescription':
      'Publishing freezes the current document as an immutable version you can roll back to at any time.',
    'workflows.version.versionName': 'Version name (optional)',
    'workflows.version.versionPlaceholder': 'e.g. v1.0 release notes',
    'workflows.version.publishedAt': 'Published {{time}}',
    'workflows.version.historyEmpty': 'No published versions yet. Publish one to enable rollbacks.',
    'workflows.version.historySearch': 'Search version name or number',
    'workflows.version.restore': 'Roll back',
    'workflows.version.restoreTitle': 'Roll back to version {{version}}?',
    'workflows.version.restoreDescription':
      'Rolling back replaces the current canvas with the graph frozen in that version; unsaved changes are lost.',
    'workflows.version.rollbackNamed': 'Roll back to version {{version}}',
    'workflows.tabs.label': 'Workflow sections',
    'workflows.tabs.editor': 'Edit',
    'workflows.tabs.versions': 'Versions',
    'workflows.draftNavigation.title': 'The draft could not be saved',
    'workflows.draftNavigation.description':
      'Your changes are not saved. Stay in the editor to resolve this, or discard the unsaved changes and leave.',
    'workflows.draftNavigation.stay': 'Stay in editor',
    'workflows.draftNavigation.leave': 'Discard changes and leave',
    'workflows.runs.loading': 'Loading runs…',
    'workflows.runs.emptyTitle': 'No runs yet',
    'workflows.runs.emptyDescription':
      'Publish a version, then press Run in Edit; every run is listed here.',
    'workflows.runs.failedTitle': 'The runs could not be loaded',
    'workflows.runs.failedDescription':
      'This read failed and can be retried. It does not mean the workflow has never run.',
    'workflows.runs.forbiddenTitle': 'No permission to read the runs',
    'workflows.runs.forbiddenDescription':
      'Your role in this space cannot read this workflow’s run history.',
    'workflows.runs.retry': 'Retry',
    'workflows.runs.loadMore': 'Load older runs',
    'workflows.runs.loadingMore': 'Loading…',
    'workflows.runs.loadMoreFailed': 'Older runs could not be loaded.',
    'workflows.runs.endOfHistory': 'This is the first run.',
    'workflows.runs.open': 'Open run {{name}}',
    'workflows.runs.version': 'Version {{id}}',
    'workflows.runs.started': 'Started {{time}}',
    'workflows.runs.finished': 'Finished {{time}}',
    'workflows.runs.notStarted': 'Not started (created {{time}})',
    'workflows.runs.notFinished': 'Not finished yet',
  },
}
