import { useSession } from '@/features/auth/session'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { useGitCredentials } from './api'

/** Bind only credentials the current member may manage; Cloud rechecks scope and availability. */
export function CredentialSelect({
  value,
  onChange,
}: {
  value: string
  onChange: (id: string) => void
}) {
  const list = useGitCredentials()
  const { session } = useSession()
  const { space } = useCurrentSpace()
  const options =
    list.data?.items.filter(
      (c) =>
        c.secretState === 'stored' &&
        c.availability === 'available' &&
        (c.attribution === 'team'
          ? space?.role === 'admin'
          : session.status === 'signed-in' && c.ownerUserId === session.user.id),
    ) ?? []
  return (
    <label className="block text-sm">
      Git 凭据
      <select
        aria-label="Git 凭据"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="ml-2 rounded border p-2"
      >
        <option value="">公开仓库（匿名读取）</option>
        {options.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name} · {c.repositoryScope}
          </option>
        ))}
      </select>
      {list.isError && <span role="alert">凭据加载失败</span>}
    </label>
  )
}
