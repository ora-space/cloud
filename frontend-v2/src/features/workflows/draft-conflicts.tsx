import { useState } from "react";
import { Button } from "@/components/ui/button";
import type { WorkflowContracts } from "@/lib/workflows/data/contracts-client";
import {
  settleDraftConflict,
  type DraftConflict,
  type DraftConflictChoice,
} from "@/lib/workflows/data/pending-drafts";
import { toast } from "@/lib/workflows/host/ui";

/**
 * Asks, before the editor opens, what to do with each draft a page left unsaved whose workflow
 * someone else changed since. Neither side is written over until the user picks: the editor would
 * otherwise show the server's draft and its next autosave would drop the kept edits for good.
 */
export function DraftConflicts({
  tenantId,
  contracts,
  conflicts,
  onSettled,
}: {
  tenantId: string;
  contracts: WorkflowContracts;
  conflicts: DraftConflict[];
  /** Called once every conflict is settled. */
  onSettled: () => void;
}) {
  const [left, setLeft] = useState(conflicts);
  const [busy, setBusy] = useState<string | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});

  async function settle(conflict: DraftConflict, choice: DraftConflictChoice) {
    const id = conflict.draft.workflowId;
    setBusy(id);
    try {
      const created = await settleDraftConflict(tenantId, contracts, conflict, choice);
      if (created !== null) toast.success(`已另存为「${created}」`);
      const rest = left.filter((c) => c.draft.workflowId !== id);
      setLeft(rest);
      if (rest.length === 0) onSettled();
    } catch {
      setErrors((current) => ({ ...current, [id]: "处理失败，请重试" }));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <div className="flex h-12 shrink-0 items-center px-4">
        <h1 className="text-sm font-semibold">工作流</h1>
      </div>
      <div className="flex flex-1 flex-col items-center gap-3 overflow-y-auto p-4">
        {left.map((conflict) => {
          const id = conflict.draft.workflowId;
          return (
            <section
              key={id}
              aria-label={`「${conflict.workflowName}」离开前的修改`}
              className="flex w-full max-w-lg flex-col gap-3 rounded-md border bg-background p-4 text-sm"
            >
              <p className="font-medium">「{conflict.workflowName}」有离开前没保存的修改</p>
              <p className="text-muted-foreground">
                上次离开时这些修改还没保存到服务端，之后服务端的草稿又被改过（可能是团队里的其他人）。两边都还在，选择怎么处理你的修改：
              </p>
              {errors[id] && (
                <p role="alert" className="text-destructive">
                  {errors[id]}
                </p>
              )}
              <div className="flex flex-wrap justify-end gap-1.5">
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy !== null}
                  onClick={() => void settle(conflict, "discard")}
                >
                  丢弃我的修改
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy !== null}
                  onClick={() => void settle(conflict, "overwrite")}
                >
                  覆盖服务端草稿
                </Button>
                <Button
                  size="sm"
                  disabled={busy !== null}
                  onClick={() => void settle(conflict, "copy")}
                >
                  另存为新工作流
                </Button>
              </div>
            </section>
          );
        })}
      </div>
    </div>
  );
}
