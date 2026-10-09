import { NavLink, Outlet } from 'react-router-dom'
import { PageHeader } from '@/components/layout/page-header'
import { cn } from '@/lib/utils'
import { workspacePaths } from '@/lib/paths'

export function SettingsLayout({ slug }: { slug: string }) {
  const p = workspacePaths(slug)
  const tabs = [
    { to: p.settings, label: '通用', end: true },
    { to: p.members, label: '成员', end: false },
    { to: p.billing, label: '账单', end: false },
    { to: p.gitIdentity, label: 'Git 身份', end: false },
  ]

  return (
    <div className="flex h-full flex-col">
      <PageHeader title="设置" />
      <div className="flex min-h-0 flex-1">
        <nav className="w-48 shrink-0 border-r p-3">
          <ul className="space-y-0.5">
            {tabs.map((tab) => (
              <li key={tab.label}>
                <NavLink
                  to={tab.to}
                  end={tab.end}
                  className={({ isActive }) =>
                    cn(
                      'block rounded-md px-2.5 py-1.5 text-sm text-muted-foreground hover:bg-muted',
                      isActive && 'bg-muted text-foreground',
                    )
                  }
                >
                  {tab.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        <div className="min-h-0 flex-1 overflow-y-auto">
          <Outlet context={slug} />
        </div>
      </div>
    </div>
  )
}
