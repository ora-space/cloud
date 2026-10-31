import { CheckIcon, ChevronDownIcon, ChevronRightIcon, CloudIcon, FolderIcon, FolderPlusIcon, LaptopIcon } from 'lucide-react'
import { type ReactNode, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandSeparator, CommandShortcut } from '@/components/ui/command'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import type { CreateProject } from '@/lib/projects-api/generated'
import type { NewChat, Project } from '../store'
import { NewProjectDialog } from './new-project-dialog'
import { SandboxLabel } from './sandbox-label'

const tab =
  'group/context h-6 min-w-0 gap-1 rounded-md px-1.5 text-xs font-normal text-muted-foreground hover:bg-muted/60 hover:text-foreground data-popup-open:bg-muted/60 data-popup-open:text-foreground'
const item = 'gap-1.5 px-2 py-1.5 text-xs'
const icon = 'size-3.5 text-muted-foreground'
const heading = '**:[[cmdk-group-heading]]:font-normal'

function TabChevron() {
  return <ChevronDownIcon className="size-3 opacity-50 transition-transform group-data-popup-open/context:rotate-180" aria-hidden />
}

/** A context tab whose menu searches its items, as desktop's project tab does. */
function SearchTab({ label, trigger, placeholder, empty, children }: { label: string; trigger: ReactNode; placeholder: string; empty: string; children: (close: () => void) => ReactNode }) {
  const [open, setOpen] = useState(false)
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger render={<Button type="button" variant="ghost" size="sm" className={tab} aria-label={label} />}>
        {trigger}
        <TabChevron />
      </PopoverTrigger>
      <PopoverContent align="start" side="top" className="w-60 p-0">
        <Command>
          <CommandInput placeholder={placeholder} aria-label={placeholder} className="text-xs" />
          <CommandList>
            <CommandEmpty className="py-4 text-xs">{empty}</CommandEmpty>
            {children(() => setOpen(false))}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}

/**
 * The row above a new chat's input, as in desktop: which project it starts in and where it runs.
 * Only the cloud runs agents from the web, and it gives every conversation a sandbox of its own,
 * which a label says.
 */
export function ContextBar({
  value,
  projects,
  onChange,
  onCreateProject,
}: {
  value: NewChat
  /** Projects a chat can start in. */
  projects: Project[]
  onChange: (patch: Partial<NewChat>) => void
  /** Creates a project from a repository, resolving to why it failed; the menu offers it only when given. */
  onCreateProject?: (input: CreateProject) => Promise<string | undefined>
}) {
  const [creating, setCreating] = useState(false)
  const project = projects.find((p) => p.id === value.projectId)

  return (
    <div className="flex min-w-0 flex-1 items-center gap-1">
      <SearchTab
        label="选择项目"
        placeholder="搜索项目"
        empty="未找到项目。"
        trigger={
          <>
            <FolderIcon className="size-3.5" />
            <span className="max-w-28 min-w-0 truncate sm:max-w-40">{project?.name ?? '选择项目'}</span>
          </>
        }
      >
        {(close) => (
          <>
            <CommandGroup className={heading}>
              {projects.map((p) => (
                <CommandItem
                  key={p.id}
                  value={`${p.name} ${p.path ?? ''}`}
                  data-checked={p.id === value.projectId}
                  className={item}
                  onSelect={() => (onChange({ projectId: p.id }), close())}
                >
                  <FolderIcon className={icon} />
                  <span className="truncate">{p.name}</span>
                </CommandItem>
              ))}
            </CommandGroup>
            {onCreateProject && (
              <>
                <CommandSeparator />
                <CommandGroup>
                  <CommandItem value="新建项目" className={item} onSelect={() => (close(), setCreating(true))}>
                    <FolderPlusIcon className={icon} />
                    新建项目
                    <CommandShortcut>
                      <ChevronRightIcon className="size-3.5" />
                    </CommandShortcut>
                  </CommandItem>
                </CommandGroup>
              </>
            )}
          </>
        )}
      </SearchTab>

      <DropdownMenu>
        <DropdownMenuTrigger render={<Button type="button" variant="ghost" size="sm" className={`${tab} shrink-0`} aria-label="选择启动模式" />}>
          <CloudIcon className="size-3.5" />
          云端
          <TabChevron />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" side="top" className="w-44">
          <DropdownMenuGroup>
            <DropdownMenuLabel className="px-2 py-1.5 text-xs font-normal text-muted-foreground">启动模式</DropdownMenuLabel>
            <DropdownMenuItem className={item} disabled>
              <LaptopIcon className={icon} />
              本地
              <span className="ml-auto text-muted-foreground">需要 desktop</span>
            </DropdownMenuItem>
            <DropdownMenuItem className={item}>
              <CloudIcon className={icon} />
              云端
              <CheckIcon className="ml-auto size-4" />
            </DropdownMenuItem>
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      <SandboxLabel />

      {onCreateProject && <NewProjectDialog open={creating} onOpenChange={setCreating} onCreate={onCreateProject} />}
    </div>
  )
}
