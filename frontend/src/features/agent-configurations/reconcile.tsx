import { Button } from '@/components/ui/button'
import type { AgentConfiguration } from './api'

/** Requires an explicit choice before a stale draft adopts the server's newer write conditions. */
export function AgentConfigurationReconcile({
  latest,
  onChoose,
}: {
  latest: AgentConfiguration
  onChoose: (keepDraft: boolean) => void
}) {
  return (
    <section aria-label="重新核对配置" className="space-y-3 rounded-md border p-3">
      <h3 className="text-sm font-medium">最新保存版本 {latest.revision}</h3>
      <p className="text-xs text-muted-foreground">
        当前草稿仍保留。请比较最新配置，再选择如何继续；选择后仍需手动保存。
      </p>
      <dl className="space-y-1 text-xs">
        {Object.entries(latest.settings).map(([name, value]) => (
          <div key={name}>
            <dt className="font-medium">{name}</dt>
            <dd className="break-all">{latest.numericValues?.[name] ?? String(value)}</dd>
          </div>
        ))}
      </dl>
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" onClick={() => onChoose(true)}>
          保留草稿，按最新要求核对
        </Button>
        <Button type="button" size="sm" variant="outline" onClick={() => onChoose(false)}>
          采用服务器配置
        </Button>
      </div>
    </section>
  )
}
