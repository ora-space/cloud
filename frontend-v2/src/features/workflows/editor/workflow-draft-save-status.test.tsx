import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ComponentProps } from "react";
import { AppI18nProvider } from "@/lib/workflows/test/i18n";
import { WorkflowDraftSaveStatusLabel } from "./workflow-draft-save-status";

/** Renders the save-status label under the same i18n provider the editor uses. */
function renderLabel(
  props: ComponentProps<typeof WorkflowDraftSaveStatusLabel>,
) {
  return render(
    <AppI18nProvider>
      <WorkflowDraftSaveStatusLabel {...props} />
    </AppI18nProvider>,
  );
}

describe("WorkflowDraftSaveStatusLabel", () => {
  it("shows saving and live-saved copy for the matching autosave states", () => {
    const { rerender } = renderLabel({ status: "saving" });
    expect(screen.getByText("保存中…")).toBeInTheDocument();

    rerender(
      <AppI18nProvider>
        <WorkflowDraftSaveStatusLabel
          status="clean"
          draftUpdatedAt="8月7日 15:42:15"
        />
      </AppI18nProvider>,
    );
    expect(
      screen.getByText("已实时保存 最近修改时间：8月7日 15:42:15"),
    ).toBeInTheDocument();
  });

  it("says a write is in flight instead of vouching for the last save", () => {
    renderLabel({ status: "saving", draftUpdatedAt: "8月7日 15:42:15" });
    expect(screen.getByText("保存中…")).toBeInTheDocument();
    expect(screen.queryByText(/已实时保存/)).not.toBeInTheDocument();
  });

  it("says edits are unsaved while they wait for the quiet period", () => {
    renderLabel({ status: "dirty", draftUpdatedAt: "8月7日 15:42:15" });
    expect(screen.getByText("未保存")).toBeInTheDocument();
    expect(screen.queryByText(/已实时保存/)).not.toBeInTheDocument();
  });

  it("surfaces save failures distinctly from the live-saved label", () => {
    renderLabel({ status: "error", draftUpdatedAt: "8月7日 15:42:15" });
    expect(screen.getByText("保存工作流失败。")).toBeInTheDocument();
    expect(screen.getByText("保存工作流失败。")).toHaveClass(
      "text-destructive",
    );
  });
});
