import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { SparklesIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useContractsClient } from "@/lib/workflows/data/contracts-client";
import { invalidateWorkflowQueries } from "@/lib/workflows/data/workflows";
import { addSampleWorkflows } from "@/lib/workflows/samples/seed";
import { toast } from "@/lib/workflows/host/ui";

/**
 * Adds the sample workflows on request. They are no longer put into every library on a first
 * visit: the library is shared by the tenant, and the samples name a demo executor.
 */
export function AddSamplesButton() {
  const contracts = useContractsClient().workflow;
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState(false);
  return (
    <Button
      variant="ghost"
      size="icon-xs"
      aria-label="添加样例工作流"
      title="添加样例工作流"
      disabled={busy}
      onClick={async () => {
        setBusy(true);
        try {
          const result = await addSampleWorkflows(contracts);
          let message = `添加了 ${result.added.length + result.unpublished.length} 个样例`;
          if (result.unpublished.length)
            message += `，其中 ${result.unpublished.length} 个未能发布，保留为草稿`;
          if (result.skipped.length) message += `；${result.skipped.length} 个同名已存在，跳过`;
          toast.success(message);
        } catch {
          toast.error("添加样例失败，请重试");
        } finally {
          setBusy(false);
          void invalidateWorkflowQueries(queryClient);
        }
      }}
    >
      <SparklesIcon />
    </Button>
  );
}
