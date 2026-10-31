import { useState } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { useGetApiV1MeTenants } from "@/api/me/me";
import { I18nextProvider } from "react-i18next";
import "@xyflow/react/dist/style.css";
import { Toaster } from "@/components/ui/sonner";
import {
  createContractsClient,
  createHttpWorkflowContracts,
} from "@/lib/workflows/data/contracts-client";
import { ContractsClientProvider } from "@/lib/workflows/data/contracts-client-provider";
import { createWorkflowQueryClient } from "@/lib/workflows/data/query-client";
import { WorkflowEditor } from "./editor/workflow-editor";
import { workflowI18n } from "@/lib/workflows/host/i18n";
import { useDarkTheme } from "@/lib/workflows/host/theme";
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

function TenantWorkflows({ tenantId }: { tenantId: string }) {
  const [client] = useState(() =>
    createContractsClient(createHttpWorkflowContracts(tenantId)),
  );
  const [queryClient] = useState(createWorkflowQueryClient);
  return (
    <I18nextProvider i18n={workflowI18n}>
      <QueryClientProvider client={queryClient}>
        <ContractsClientProvider client={client}>
          <WorkflowWorkspace />
          <WorkflowToaster />
        </ContractsClientProvider>
      </QueryClientProvider>
    </I18nextProvider>
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
