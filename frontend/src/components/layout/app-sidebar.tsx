import {
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
  Server,
  Sparkles,
  Users,
} from 'lucide-react'
import { useState } from 'react'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import type { User } from '@/api/generated.schemas'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
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
import type { SpaceListItem } from '@/api/generated.schemas'
import { useLogout } from '@/features/auth/api'
import { useInboxItems } from '@/features/inbox/api'
import { CreateSpaceDialog } from '@/features/spaces/create-space-dialog'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { workspacePaths } from '@/lib/paths'
import { db, workspaceBySlug } from '@/mocks/data/store'

const workNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.issues, label: '任务', icon: Layers },
  { to: (p: ReturnType<typeof workspacePaths>) => p.projects, label: '项目', icon: CircuitBoard },
]

const aiTeamNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.agents, label: '智能体', icon: Bot },
  { to: (p: ReturnType<typeof workspacePaths>) => p.squads, label: '小队', icon: Users },
  { to: (p: ReturnType<typeof workspacePaths>) => p.skills, label: '技能', icon: Sparkles },
  { to: (p: ReturnType<typeof workspacePaths>) => p.runtimes, label: '运行时', icon: Server },
]

const utilityNav = [
  { to: (p: ReturnType<typeof workspacePaths>) => p.settings, label: '设置', icon: Cog },
]

/** Workspaces the switcher lists: cloud lists only joined real spaces. */
function switchableSpaces(cloudMode: boolean, spaces: SpaceListItem[] | undefined) {
  if (cloudMode) return spaces ?? []
  return db.workspaces
}

// oxlint-disable-next-line max-lines-per-function -- this composition root owns the complete sidebar navigation tree.
export function AppSidebar({ slug, user }: { slug: string; user: User }) {
  const p = workspacePaths(slug)
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const logout = useLogout()
  const { cloudMode, spaces, tenantId, space } = useCurrentSpace()
  const { data: inboxItems = [] } = useInboxItems(slug)
  const unreadCount = inboxItems.filter((i) => !i.read).length
  const activeWorkspace = space ?? workspaceBySlug(slug) ?? db.workspace
  // Cloud sessions switch between real spaces (collaboration pages), mock
  // sessions between the demo store workspaces.
  const switchable = switchableSpaces(cloudMode, spaces)
  const [createSpaceOpen, setCreateSpaceOpen] = useState(false)

  function handleLogout() {
    logout.mutate(undefined, { onSuccess: () => void navigate('/login') })
  }

  return (
    <Sidebar variant="inset">
      <CreateSpaceDialog
        open={createSpaceOpen}
        onOpenChange={setCreateSpaceOpen}
        tenantId={tenantId}
        onCreated={(newSlug) => void navigate(`/${newSlug}/projects`)}
      />
      <SidebarHeader className="py-3">
        <SidebarMenu>
          <SidebarMenuItem>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <SidebarMenuButton>
                    <span
                      className="flex size-5 items-center justify-center rounded-sm text-[11px] font-semibold text-white"
                      style={{
                        backgroundColor:
                          'avatarColor' in activeWorkspace
                            ? activeWorkspace.avatarColor
                            : '#3b82f6',
                      }}
                    >
                      {activeWorkspace.name.charAt(0)}
                    </span>
                    <span className="flex-1 truncate font-medium">{activeWorkspace.name}</span>
                    <ChevronDown className="size-3 text-muted-foreground" />
                  </SidebarMenuButton>
                }
              />
              <DropdownMenuContent className="w-56" align="start" side="bottom" sideOffset={4}>
                <div className="flex items-center gap-2.5 px-2 py-1.5">
                  <Avatar className="size-10 text-sm">
                    <AvatarFallback className="bg-primary font-medium text-primary-foreground">
                      {(user.displayName || user.id).charAt(0).toUpperCase()}
                    </AvatarFallback>
                  </Avatar>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium leading-tight">
                      {user.displayName || user.id}
                    </p>
                    <p className="truncate text-xs text-muted-foreground leading-tight">
                      已通过统一身份认证
                    </p>
                  </div>
                </div>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="text-xs text-muted-foreground">
                    工作区
                  </DropdownMenuLabel>
                  {switchable.map((ws) => (
                    <DropdownMenuItem
                      key={ws.id}
                      onClick={() => {
                        // Cloud spaces have no mock issue boards; land on projects.
                        const target = cloudMode ? 'projects' : 'issues'
                        if (ws.slug !== slug) void navigate(`/${ws.slug}/${target}`)
                      }}
                    >
                      <span
                        className="flex size-5 items-center justify-center rounded-sm text-[10px] font-semibold text-white"
                        style={{
                          backgroundColor: 'avatarColor' in ws ? ws.avatarColor : '#3b82f6',
                        }}
                      >
                        {ws.name.charAt(0)}
                      </span>
                      <span className="flex-1 truncate">{ws.name}</span>
                      {ws.slug === slug && <Check className="size-3.5" />}
                    </DropdownMenuItem>
                  ))}
                  {cloudMode && (
                    <DropdownMenuItem onClick={() => setCreateSpaceOpen(true)}>
                      <Plus className="size-3.5" />
                      新建工作区
                    </DropdownMenuItem>
                  )}
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  variant="destructive"
                  disabled={logout.isPending}
                  onClick={handleLogout}
                >
                  <LogOut className="size-3.5" />
                  {logout.isPending ? '正在退出…' : '退出登录'}
                </DropdownMenuItem>
                {logout.isError && (
                  <p className="px-2 py-1 text-xs text-destructive">退出失败，请重试</p>
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

        {!cloudMode && (
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
        )}
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
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
