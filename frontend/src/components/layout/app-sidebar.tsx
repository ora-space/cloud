import {
  Activity,
  Bot,
  Check,
  ChevronDown,
  CircuitBoard,
  Cog,
  Inbox,
  Layers,
  ListTodo,
  LogOut,
  MessageCircle,
  Plus,
  Puzzle,
  Server,
  Sparkles,
  Users,
  Workflow,
} from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import { useLoginProviders } from '@/features/auth/providers'
import { useSession } from '@/features/auth/session'
import { useInboxItems } from '@/features/inbox/api'
import { CreateSpaceDialog } from '@/features/spaces/create-space-dialog'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { activeLocale, setLocale } from '@/i18n/i18n-instance'
import { LOCALES, type Locale } from '@/i18n/locale'
import { workspacePaths } from '@/lib/paths'

const workNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.issues, label: '任务', icon: Layers },
  { to: (p: ReturnType<typeof workspacePaths>) => p.projects, label: '项目', icon: CircuitBoard },
  { to: (p: ReturnType<typeof workspacePaths>) => p.workflows, label: '工作流', icon: Workflow },
  { to: (p: ReturnType<typeof workspacePaths>) => p.sessions, label: '会话分析', icon: Activity },
]

const aiTeamNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.agents, label: '智能体', icon: Bot },
  { to: (p: ReturnType<typeof workspacePaths>) => p.squads, label: '小队', icon: Users },
  { to: (p: ReturnType<typeof workspacePaths>) => p.skills, label: '技能', icon: Sparkles },
  { to: (p: ReturnType<typeof workspacePaths>) => p.plugins, label: '插件', icon: Puzzle },
  { to: (p: ReturnType<typeof workspacePaths>) => p.runtimes, label: '运行时', icon: Server },
]

const utilityNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.settings, label: '设置', icon: Cog },
]

/**
 * Each language named in itself, so a member who cannot read the current
 * interface language can still find their own — which is also why these labels
 * are literals rather than translation keys.
 */
const LOCALE_LABELS: Record<Locale, string> = { 'zh-CN': '中文', 'en-US': 'English' }

/**
 * Switches the interface language. Reads the applied locale from the shared
 * i18n instance and subscribes through `useTranslation()` so the highlight
 * follows a switch made anywhere else.
 */
function LanguageToggle() {
  // The subscription: react-i18next re-renders this component on a language
  // change, which is what keeps the selected state in step.
  useTranslation()
  const current = activeLocale()
  return (
    <div className="mt-1 flex items-center gap-1 px-2 pb-1">
      {LOCALES.map((locale) => (
        <button
          key={locale}
          type="button"
          onClick={() => setLocale(locale)}
          aria-pressed={current === locale}
          className={
            current === locale
              ? 'rounded px-1.5 py-0.5 text-[11px] font-medium text-foreground'
              : 'rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:text-foreground'
          }
        >
          {LOCALE_LABELS[locale]}
        </button>
      ))}
    </div>
  )
}

// oxlint-disable-next-line max-lines-per-function -- this composition root owns the complete sidebar navigation tree.
export function AppSidebar({ slug }: { slug: string }) {
  const p = workspacePaths(slug)
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const { session, signOut, signOutOfGitHub } = useSession()
  const { data: providers = [] } = useLoginProviders()
  const user = session.status === 'signed-in' ? session.user : undefined
  const { spaces = [], space } = useCurrentSpace()
  const { data: inboxItems = [] } = useInboxItems(slug)
  const unreadCount = inboxItems.filter((i) => !i.read).length
  const [createSpaceOpen, setCreateSpaceOpen] = useState(false)
  // The layout only renders the sidebar once the slug resolved, so a missing
  // space is a programming error rather than a state to design for.
  if (!space) throw new Error('AppSidebar requires a resolved space')

  return (
    <Sidebar variant="inset">
      <CreateSpaceDialog
        open={createSpaceOpen}
        onOpenChange={setCreateSpaceOpen}
        onCreated={(newSlug) => void navigate(workspacePaths(newSlug).issues)}
      />
      <SidebarHeader className="py-3">
        <SidebarMenu>
          <SidebarMenuItem>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <SidebarMenuButton>
                    <span className="flex size-5 items-center justify-center rounded-sm bg-primary text-[11px] font-semibold text-primary-foreground">
                      {space.name.charAt(0)}
                    </span>
                    <span className="flex-1 truncate font-medium">{space.name}</span>
                    <ChevronDown className="size-3 text-muted-foreground" />
                  </SidebarMenuButton>
                }
              />
              <DropdownMenuContent className="w-56" align="start" side="bottom" sideOffset={4}>
                <div className="flex items-center gap-2.5 px-2 py-1.5">
                  <span className="flex size-7 items-center justify-center rounded-full bg-muted text-xs font-medium">
                    {user?.displayName.charAt(0) ?? '?'}
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium leading-tight">
                      {user?.displayName}
                    </p>
                  </div>
                </div>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="text-xs text-muted-foreground">
                    工作区
                  </DropdownMenuLabel>
                  {spaces.map((ws) => (
                    <DropdownMenuItem
                      key={ws.id}
                      onClick={() => {
                        if (ws.slug !== slug) void navigate(workspacePaths(ws.slug).issues)
                      }}
                    >
                      <span className="flex size-5 items-center justify-center rounded-sm bg-primary text-[10px] font-semibold text-primary-foreground">
                        {ws.name.charAt(0)}
                      </span>
                      <span className="flex-1 truncate">{ws.name}</span>
                      {ws.slug === slug && <Check className="size-3.5" />}
                    </DropdownMenuItem>
                  ))}
                  <DropdownMenuItem onClick={() => setCreateSpaceOpen(true)}>
                    <Plus className="size-3.5" />
                    新建工作区
                  </DropdownMenuItem>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuItem variant="destructive" onClick={() => void signOut()}>
                  <LogOut className="size-3.5" />
                  退出登录
                </DropdownMenuItem>
                {providers.includes('github') && (
                  <DropdownMenuItem variant="destructive" onClick={() => void signOutOfGitHub()}>
                    <LogOut className="size-3.5" />
                    退出并注销 GitHub
                  </DropdownMenuItem>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupContent>
            <SidebarMenu className="gap-0.5">
              <SidebarMenuItem>
                <SidebarMenuButton
                  isActive={pathname === p.inbox}
                  render={<NavLink to={p.inbox} />}
                >
                  <Inbox />
                  <span>收件箱</span>
                  {unreadCount > 0 && (
                    <span className="ml-auto rounded-full bg-primary px-1.5 text-[10px] text-primary-foreground">
                      {unreadCount}
                    </span>
                  )}
                </SidebarMenuButton>
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuButton
                  isActive={pathname === p.myIssues}
                  render={<NavLink to={p.myIssues} />}
                >
                  <ListTodo />
                  <span>我的任务</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuButton
                  isActive={pathname.startsWith(p.chat)}
                  render={<NavLink to={p.chat} />}
                >
                  <MessageCircle />
                  <span>聊天</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>工作</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu className="gap-0.5">
              {workNav.map((item) => {
                const href = item.to(p)
                const Icon = item.icon
                return (
                  <SidebarMenuItem key={item.label}>
                    <SidebarMenuButton
                      isActive={pathname.startsWith(href)}
                      render={<NavLink to={href} />}
                    >
                      <Icon />
                      <span>{item.label}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                )
              })}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>AI 团队</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu className="gap-0.5">
              {aiTeamNav.map((item) => {
                const href = item.to(p)
                const Icon = item.icon
                return (
                  <SidebarMenuItem key={item.label}>
                    <SidebarMenuButton
                      isActive={pathname.startsWith(href)}
                      render={<NavLink to={href} />}
                    >
                      <Icon />
                      <span>{item.label}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                )
              })}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="p-2">
        <SidebarMenu className="gap-0.5">
          {utilityNav.map((item) => {
            const href = item.to(p)
            const Icon = item.icon
            return (
              <SidebarMenuItem key={item.label}>
                <SidebarMenuButton
                  isActive={pathname.startsWith(href)}
                  render={<NavLink to={href} />}
                >
                  <Icon />
                  <span>{item.label}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            )
          })}
        </SidebarMenu>
        <LanguageToggle />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
