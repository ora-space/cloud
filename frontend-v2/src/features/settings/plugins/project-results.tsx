import { useGetSkillProgress } from '../market-api/generated'
const labels: Record<string, string> = {
  waiting: '等待节点',
  applying: '处理中',
  installed: '已安装',
  removed: '已移除',
  failed: '失败',
}
export function ProjectResults({
  tenant,
  space,
  namespace,
  identifier,
}: {
  tenant: string
  space: string
  namespace: string
  identifier: string
}) {
  const results = useGetSkillProgress(tenant, space, namespace, identifier, {
    query: { refetchInterval: 3000 },
  })
  return (
    <section className="space-y-2">
      <h3 className="font-medium">项目节点</h3>
      {results.isLoading && <p role="status">正在读取节点状态…</p>}
      {results.isError && <p role="alert">未能读取项目结果。</p>}
      {results.data?.items.length === 0 && (
        <p className="text-muted-foreground">当前空间还没有关联项目。</p>
      )}
      {results.data?.items.map((p) => (
        <div key={p.projectId} className="rounded border p-2">
          <strong>{p.name}</strong> · {labels[p.state] ?? p.state}
          {p.installedVersion && <span> · v{p.installedVersion}</span>}
          {p.failure && <p role="alert">{p.failure}</p>}
        </div>
      ))}
    </section>
  )
}
