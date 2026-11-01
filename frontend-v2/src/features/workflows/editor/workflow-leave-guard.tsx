import { useContext, useEffect } from "react";
import { useTranslation } from "react-i18next";
import { UNSAFE_DataRouterContext, useBlocker } from "react-router";
import type { WorkflowDraftSaveStatus } from "./use-workflow-draft-autosave";

type WorkflowLeaveGuardProps = {
  status: WorkflowDraftSaveStatus;
  /** Writes pending edits; resolves false when they could not reach the server. */
  flush: () => Promise<boolean>;
};

/**
 * Keeps unsaved draft edits from being lost silently when the user leaves the editor. A reload
 * or tab close asks the browser to confirm; leaving through the app first tries to save and asks
 * only if that fails. Nothing is kept in the browser: the server draft is the only copy.
 */
export function WorkflowLeaveGuard({ status, flush }: WorkflowLeaveGuardProps) {
  const pending = status !== "clean";
  useEffect(() => {
    if (!pending) {
      return;
    }
    const confirmUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
    };
    window.addEventListener("beforeunload", confirmUnload);
    return () => window.removeEventListener("beforeunload", confirmUnload);
  }, [pending]);
  // Route blocking needs a data router; embedded renders (tests, previews) have none.
  const dataRouter = useContext(UNSAFE_DataRouterContext);
  return dataRouter === null ? null : <RouteLeaveGuard pending={pending} flush={flush} />;
}

function RouteLeaveGuard({ pending, flush }: { pending: boolean; flush: () => Promise<boolean> }) {
  const { t } = useTranslation();
  const blocker = useBlocker(pending);
  useEffect(() => {
    if (blocker.state !== "blocked") {
      return;
    }
    let cancelled = false;
    void flush()
      .catch(() => false)
      .then((saved) => {
        if (cancelled || blocker.state !== "blocked") {
          return;
        }
        if (saved || window.confirm(t("settings.workflow.leaveUnsaved"))) {
          blocker.proceed();
        } else {
          blocker.reset();
        }
      });
    return () => {
      cancelled = true;
    };
  }, [blocker, flush, t]);
  return null;
}
