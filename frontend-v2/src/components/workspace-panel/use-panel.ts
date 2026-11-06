import { useCallback, useMemo, useState } from 'react'
import type { WorkspaceNavigation } from './navigation'
import type { OpenRequest } from './workspace-panel'

const storageKey = 'ora.v2.workspace-panel.open'

function remembered(): boolean {
  try {
    return localStorage.getItem(storageKey) === '1'
  } catch {
    return false
  }
}

/**
 * Whether a page's workspace panel is open, remembered per browser so it stays as the person left
 * it, plus the navigation that opens it on one file's diff.
 */
export function useWorkspacePanel() {
  const [open, setOpenState] = useState(remembered)
  const [request, setRequest] = useState<OpenRequest>()
  const setOpen = useCallback((next: boolean) => {
    setOpenState(next)
    try {
      localStorage.setItem(storageKey, next ? '1' : '0')
    } catch {
      // A private window or blocked storage only forgets the choice.
    }
  }, [])
  const navigation = useMemo<WorkspaceNavigation>(
    () => ({
      openDiff: (path) => {
        setOpen(true)
        setRequest((r) => ({ path, nonce: (r?.nonce ?? 0) + 1 }))
      },
    }),
    [setOpen],
  )
  return { open, setOpen, request, navigation }
}

/** A page's workspace panel, for a host that keeps the toggle outside the view showing the panel. */
export type WorkspacePanelState = ReturnType<typeof useWorkspacePanel>
