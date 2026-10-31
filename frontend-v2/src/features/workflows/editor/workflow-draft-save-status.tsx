import { useTranslation } from "react-i18next";
import { cn } from "@/lib/workflows/host/ui";
import type { WorkflowDraftSaveStatus } from "./use-workflow-draft-autosave";

type WorkflowDraftSaveStatusProps = {
  status: WorkflowDraftSaveStatus;
  /** Formatted draft `updated_at` after the last successful persist. */
  draftUpdatedAt?: string;
  className?: string;
};

/**
 * Surfaces autosave progress next to the workflow title without introducing a
 * manual Save control. The live-saved timestamp shows only once the draft is
 * clean: the backend is remote, so while edits wait for the quiet period or the
 * write is in flight the label says so instead of vouching for the last save.
 */
export function WorkflowDraftSaveStatusLabel({
  status,
  draftUpdatedAt,
  className,
}: WorkflowDraftSaveStatusProps) {
  const { t } = useTranslation();

  let label: string;
  if (status === "error") {
    label = t("settings.workflow.saveError");
  } else if (status === "saving") {
    label = t("settings.workflow.saving");
  } else if (status === "dirty") {
    label = t("settings.workflow.unsaved");
  } else if (draftUpdatedAt !== undefined) {
    label = t("settings.workflow.liveSaved", { time: draftUpdatedAt });
  } else {
    label = t("settings.workflow.saved");
  }

  return (
    <p
      aria-live="polite"
      className={cn(
        "max-w-56 shrink-0 truncate text-right text-[10px] leading-4 text-muted-foreground tabular-nums",
        status === "error" && "text-destructive",
        className,
      )}
      title={label}
    >
      {label}
    </p>
  );
}
