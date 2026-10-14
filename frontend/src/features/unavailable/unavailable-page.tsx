import { Link } from 'react-router-dom'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'

/** A supported workspace route offered instead of an unavailable feature. */
interface AvailableAction {
  label: string
  to: string
}

/**
 * An unavailable product entry with explicit alternatives. This screen does not
 * read a catalog, synthesize records or accept work for the unavailable feature.
 */
export function UnavailablePage({
  title,
  description,
  actions,
}: {
  title: string
  description: string
  actions: readonly AvailableAction[]
}) {
  return (
    <div className="flex h-full flex-col">
      <PageHeader title={title} />
      <section className="space-y-4 p-6">
        <h2 className="text-base font-semibold">{title}暂不可用</h2>
        <p className="max-w-xl text-sm text-muted-foreground">{description}</p>
        <div className="flex flex-wrap gap-2">
          {actions.map((action) => (
            <Button key={action.to} variant="outline" render={<Link to={action.to} />}>
              {action.label}
            </Button>
          ))}
        </div>
      </section>
    </div>
  )
}
