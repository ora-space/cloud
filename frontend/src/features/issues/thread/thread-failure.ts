const FAILURE_LABELS: Record<string, string> = {
  agent_turn_failed: '模型请求失败，会话已结束',
  agent_stream_closed: 'Agent 连接中断，会话已结束',
  agent_authentication_required: 'Agent 未登录或缺少凭据，请先完成登录或配置凭据',
  agent_failed: 'Agent 会话失败',
  agent_unavailable: 'Agent 当前不可用',
  agent_start_failed: 'Agent 启动失败',
  model_proxy_unavailable: '模型代理不可用',
  model_access_unavailable: '模型授权不可用',
  model_access_expired: '模型授权已过期',
  thread_unavailable: '会话记录不可用',
  clone_attempts_exhausted: '获取仓库失败，自动重试次数已用尽',
  repository_sync_failed: '项目仓库同步失败，无法获取仓库',
}

/** Unknown data is replaced by a fixed code; provider diagnostics never become display text. */
export function sessionFailureMessage(code: unknown): string {
  const safe =
    typeof code === 'string' && Object.hasOwn(FAILURE_LABELS, code) ? code : 'agent_failed'
  return `${FAILURE_LABELS[safe]}（${safe}）`
}
