import { PanelLeftCloseIcon } from "lucide-react";
import {
  SidebarResizeHandle,
  type SidebarLayout,
} from "@/components/sidebar-resize-handle";
import { Button } from "@/components/ui/button";
import { WorkflowEditorList } from "./editor/workflow-editor-list";
import { AddSamplesButton, LegacyLibraryNotice } from "./library-actions";
import { useUiStore } from "@/lib/workflows/host/ui-store";

/**
 * Desktop's workflow library in a column that resizes and folds with the shared edge the chat
 * history uses. Folded, the editor header shows the button that brings it back.
 */
export function WorkflowSidebar() {
  const collapsed = useUiStore((state) => state.sidebarCollapsed);
  const width = useUiStore((state) => state.sidebarWidth);
  const { setSidebarCollapsed, setSidebarWidth } = useUiStore.getState();
  const layout: SidebarLayout = { width, open: !collapsed };
  const changeLayout = (next: Partial<SidebarLayout>) => {
    if (next.width !== undefined) setSidebarWidth(next.width);
    if (next.open !== undefined) setSidebarCollapsed(!next.open);
  };
  return (
    <>
      {!collapsed && (
        <aside
          aria-label="工作流列表"
          style={{ width }}
          className="flex shrink-0 flex-col border-r bg-sidebar text-sidebar-foreground"
        >
          <div className="flex h-12 shrink-0 items-center justify-between pr-2 pl-4">
            <h1 className="text-sm font-semibold">工作流</h1>
            <div className="flex items-center gap-0.5">
            <AddSamplesButton />
            <Button
              variant="ghost"
              size="icon-xs"
              aria-label="收起工作流列表"
              onClick={() => setSidebarCollapsed(true)}
            >
              <PanelLeftCloseIcon />
            </Button>
            </div>
          </div>
          <LegacyLibraryNotice />
          <WorkflowEditorList />
        </aside>
      )}
      <SidebarResizeHandle
        label="调整工作流列表宽度"
        layout={layout}
        onChange={changeLayout}
      />
    </>
  );
}
