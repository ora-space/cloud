const FAILURE_LABELS: Record<string, string> = {
  agent_turn_failed: '模型请求失败，会话已结束',
  agent_stream_closed: 'Agent 连接中断，会话已结束',
  agent_authentication_required: 'Agent 未登录或缺少凭据，请先完成登录或配置凭据',
  agent_failed: 'Agent 会话失败',
  agent_unavailable: 'Agent 当前不可用',
  agent_start_failed: 'Agent 启动失败',
  agent_plugin_unavailable: '运行所需的 Agent 插件未安装就绪，请检查插件安装状态后重新发送',
  model_proxy_unavailable: '模型代理不可用',
  model_access_unavailable: '模型授权不可用',
  model_access_expired: '模型授权已过期',
  thread_unavailable: '会话记录不可用',
  agent_adapter_unavailable: '执行节点不支持该 Agent 的模型配置',
  agent_model_configuration_required: '该 Agent 缺少模型配置',
  agent_model_configuration_invalid: 'Agent 模型配置与已安装的插件不一致',
  agent_model_protocol_mismatch: 'Agent 插件不支持所选模型连接的协议',
  model_grant_mismatch: '模型授权与运行配置不一致',
  agent_model_adapter_failed: 'Agent 无法应用模型配置',
  agent_model_adapter_not_applied: 'Agent 未确认已应用模型配置',
  clone_attempts_exhausted: '获取仓库失败，自动重试次数已用尽',
  repository_sync_failed: '项目仓库同步失败，无法获取仓库',
}

/** Unknown data is replaced by a fixed code; provider diagnostics never become display text. */
export function sessionFailureMessage(code: unknown): string {
  const safe =
    typeof code === 'string' && Object.hasOwn(FAILURE_LABELS, code) ? code : 'agent_failed'
  return `${FAILURE_LABELS[safe]}（${safe}）`
}
