import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { SparklesIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useContractsClient } from "@/lib/workflows/data/contracts-client";
import {
  importLegacyWorkflows,
  readLegacyWorkflows,
  browserLegacyLibraryStorage,
} from "@/lib/workflows/data/legacy-library";
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

/**
 * Offers once to bring the workflows this browser kept before v2-server stored them. Nothing is
 * moved without the user's answer, and dismissing leaves the old entry in the browser untouched.
 */
export function LegacyLibraryNotice() {
  const contracts = useContractsClient().workflow;
  const queryClient = useQueryClient();
  const [workflows, setWorkflows] = useState(() => readLegacyWorkflows());
  const [busy, setBusy] = useState(false);
  if (workflows.length === 0) return null;
  return (
    <div
      role="region"
      aria-label="本机旧工作流"
      className="mx-2 mb-2 flex flex-col gap-2 rounded-md border bg-background p-2.5 text-xs"
    >
      <p>
        这个浏览器里还有 {workflows.length} 个旧的本机工作流。导入后保存在服务端，只带草稿，旧的发布版本需要重新发布。
      </p>
      <div className="flex justify-end gap-1.5">
        <Button
          size="xs"
          variant="ghost"
          disabled={busy}
          onClick={() => {
            browserLegacyLibraryStorage.markHandled();
            setWorkflows([]);
          }}
        >
          不导入
        </Button>
        <Button
          size="xs"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            try {
              const result = await importLegacyWorkflows(contracts, workflows);
              setWorkflows([]);
              toast.success(
                result.skipped.length
                  ? `导入了 ${result.imported.length} 个；${result.skipped.length} 个同名已存在，跳过：${result.skipped.join("、")}`
                  : `导入了 ${result.imported.length} 个工作流`,
              );
            } catch {
              toast.error("导入中断，已导入的保留，可以再试一次");
            } finally {
              setBusy(false);
              void invalidateWorkflowQueries(queryClient);
            }
          }}
        >
          导入
        </Button>
      </div>
    </div>
  );
}
