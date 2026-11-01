import { useTheme } from 'next-themes'
import type {
  PluginCatalogEntry,
  PluginLogoCandidate,
} from '@/api/generated.schemas'
import { pluginKind } from './plugin-kind'

type EmbeddedLogoCandidate = PluginLogoCandidate & { data?: string }

function embeddedLogo(
  logo: PluginCatalogEntry['logo'],
  resolvedTheme: string | undefined,
) {
  if (!logo) return undefined
  const candidate = (
    'universal' in logo
      ? logo.universal
      : resolvedTheme === 'dark'
        ? logo.dark
        : logo.light
  ) as EmbeddedLogoCandidate
  return candidate.data?.startsWith('data:image/') ? candidate.data : undefined
}

export function PluginMark({
  entry,
  large = false,
}: {
  entry: Pick<PluginCatalogEntry, 'kind' | 'logo' | 'title'>
  large?: boolean
}) {
  const { resolvedTheme } = useTheme()
  const source = embeddedLogo(entry.logo, resolvedTheme)
  const meta = pluginKind(entry)
  const Fallback = meta.icon
  return (
    <span
      className={`settings-plugin-mark${large ? ' settings-plugin-mark-large' : ''}`}
      data-kind={entry.kind}
    >
      {source ? (
        <img src={source} alt="" aria-hidden="true" />
      ) : (
        <Fallback aria-hidden="true" />
      )}
    </span>
  )
}
