import { create } from 'zustand'
import { createJSONStorage, persist } from 'zustand/middleware'

const EMPTY_SELECTION: string[] = []

interface DemoPluginSelections {
  byScope: Record<string, string[]>
  setSelected: (scope: string, plugin: string, selected: boolean) => void
  reset: () => void
}

/**
 * Browser-local MCP/Hook selections used only for the v2 demo. Scoping by user, tenant and space
 * keeps one person's demo from changing another workspace or login.
 */
export const useDemoPluginSelections = create<DemoPluginSelections>()(
  persist(
    (set) => ({
      byScope: {},
      setSelected: (scope, plugin, selected) =>
        set((state) => {
          const current = state.byScope[scope] ?? EMPTY_SELECTION
          const next = selected
            ? current.includes(plugin)
              ? current
              : [...current, plugin]
            : current.filter((item) => item !== plugin)
          return { byScope: { ...state.byScope, [scope]: next } }
        }),
      reset: () => set({ byScope: {} }),
    }),
    {
      name: 'ora-v2-plugin-demo:v1',
      version: 1,
      storage: createJSONStorage(() => localStorage),
    },
  ),
)

export function demoSelectionFor(scope: string) {
  return (state: DemoPluginSelections) =>
    state.byScope[scope] ?? EMPTY_SELECTION
}
