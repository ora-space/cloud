import { useEffect, useState } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { useGetApiV1MeTenants } from "@/api/me/me";
import { I18nextProvider } from "react-i18next";
import "@xyflow/react/dist/style.css";
import { Toaster } from "@/components/ui/sonner";
import {
  browserDraftKeeper,
  createContractsClient,
  createHttpWorkflowContracts,
} from "@/lib/workflows/data/contracts-client";
import { ContractsClientProvider } from "@/lib/workflows/data/contracts-client-provider";
import {
  pendingDrafts,
  replayPendingDrafts,
  type DraftConflict,
} from "@/lib/workflows/data/pending-drafts";
import { toast } from "@/lib/workflows/host/ui";
import { createWorkflowQueryClient } from "@/lib/workflows/data/query-client";
import { WorkflowEditor } from "./editor/workflow-editor";
import { workflowI18n } from "@/lib/workflows/host/i18n";
import { useDarkTheme } from "@/lib/workflows/host/theme";
import { DraftConflicts } from "./draft-conflicts";
import { WorkflowSidebar } from "./workflow-sidebar";

/**
 * The workflow page: desktop's library sidebar and definition editor side by side. In desktop the
 * library replaces the app sidebar while the editor owns the workspace pane; here the page holds
 * both. Workflows are stored by v2-server in the user's tenant; the page keeps its own query cache
 * with desktop's policy, and starts over when the tenant changes.
 */
export default function Page() {
  const tenants = useGetApiV1MeTenants();
  const tenantId = tenants.data?.items[0]?.id;
  if (!tenantId) {
    const message = tenants.isError
      ? "读取团队失败，请刷新重试。"
      : tenants.data
        ? "当前账号不在任何团队里。"
        : "正在加载…";
    return (
      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex h-12 shrink-0 items-center px-4">
          <h1 className="text-sm font-semibold">工作流</h1>
        </div>
        <p className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
          {message}
        </p>
      </div>
    );
  }
  return <TenantWorkflows key={tenantId} tenantId={tenantId} />;
}

/**
 * One tenant's library. Drafts a closed page left unsaved are written first, so the editor
 * reads the last edits instead of the draft the server had before them; one whose workflow
 * someone else changed meanwhile is put to the user before the editor opens.
 */
function TenantWorkflows({ tenantId }: { tenantId: string }) {
  const [contracts] = useState(() => createHttpWorkflowContracts(tenantId));
  const [client] = useState(() =>
    createContractsClient(contracts, browserDraftKeeper(tenantId)),
  );
  const [queryClient] = useState(createWorkflowQueryClient);
  const [phase, setPhase] = useState<"replaying" | "ready" | DraftConflict[]>(() =>
    pendingDrafts(tenantId).length > 0 ? "replaying" : "ready",
  );
  const replaying = phase === "replaying";
  useEffect(() => {
    if (!replaying) return;
    let live = true;
    void replayPendingDrafts(tenantId, contracts).then((result) => {
      if (!live) return;
      if (result.waiting > 0) {
        toast.error(`有 ${result.waiting} 个工作流上次离开时的修改还没保存到服务端，下次打开会再试`);
      }
      if (result.refusedNames.length > 0) {
        toast.error(`标题「${result.refusedNames.join("」「")}」没能保存（已被占用或为空），图已保存`);
      }
      setPhase(result.conflicts.length > 0 ? result.conflicts : "ready");
    });
    return () => {
      live = false;
    };
  }, [contracts, replaying, tenantId]);
  let content;
  if (phase === "replaying") {
    content = (
      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex h-12 shrink-0 items-center px-4">
          <h1 className="text-sm font-semibold">工作流</h1>
        </div>
        <p className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
          正在保存上次离开时的修改…
        </p>
      </div>
    );
  } else if (phase !== "ready") {
    content = (
      <DraftConflicts
        tenantId={tenantId}
        contracts={contracts}
        conflicts={phase}
        onSettled={() => setPhase("ready")}
      />
    );
  } else {
    content = (
      <I18nextProvider i18n={workflowI18n}>
        <QueryClientProvider client={queryClient}>
          <ContractsClientProvider client={client}>
            <WorkflowWorkspace />
          </ContractsClientProvider>
        </QueryClientProvider>
      </I18nextProvider>
    );
  }
  return (
    <>
      {content}
      <WorkflowToaster />
    </>
  );
}

function WorkflowWorkspace() {
  return (
    <div className="flex min-h-0 min-w-0 flex-1">
      <WorkflowSidebar />
      <WorkflowEditor />
    </div>
  );
}

/**
 * The editor's toasts, themed by frontend-v2's `dark` class rather than a theme provider. Each one
 * has a close button, so a message that covers the canvas controls can be dismissed at once.
 */
function WorkflowToaster() {
  const dark = useDarkTheme();
  return (
    <Toaster
      position="bottom-right"
      theme={dark ? "dark" : "light"}
      closeButton
    />
  );
}
