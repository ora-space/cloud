import { act, render, screen } from "@testing-library/react";
import { createMemoryRouter, Link, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WorkflowDraftSaveStatus } from "./use-workflow-draft-autosave";
import { WorkflowLeaveGuard } from "./workflow-leave-guard";

function unload(): BeforeUnloadEvent {
  const event = new Event("beforeunload", { cancelable: true }) as BeforeUnloadEvent;
  window.dispatchEvent(event);
  return event;
}

function routed(status: WorkflowDraftSaveStatus, flush: () => Promise<boolean>) {
  const router = createMemoryRouter(
    [
      {
        path: "/workflows",
        element: (
          <>
            <WorkflowLeaveGuard status={status} flush={flush} />
            <Link to="/chat">对话</Link>
          </>
        ),
      },
      { path: "/chat", element: <p>对话页</p> },
    ],
    { initialEntries: ["/workflows"] },
  );
  render(<RouterProvider router={router} />);
  return router;
}

afterEach(() => vi.restoreAllMocks());

describe("WorkflowLeaveGuard", () => {
  it("asks the browser to confirm a reload only while edits are unsaved", () => {
    const { rerender } = render(<WorkflowLeaveGuard status="clean" flush={async () => true} />);
    expect(unload().defaultPrevented).toBe(false);
    rerender(<WorkflowLeaveGuard status="dirty" flush={async () => true} />);
    expect(unload().defaultPrevented).toBe(true);
    rerender(<WorkflowLeaveGuard status="saving" flush={async () => true} />);
    expect(unload().defaultPrevented).toBe(true);
  });

  it("saves before leaving through the app and leaves without asking", async () => {
    const confirm = vi.spyOn(window, "confirm");
    const flush = vi.fn(async () => true);
    const router = routed("dirty", flush);
    await act(async () => screen.getByRole("link", { name: "对话" }).click());
    expect(await screen.findByText("对话页")).toBeInTheDocument();
    expect(flush).toHaveBeenCalledOnce();
    expect(confirm).not.toHaveBeenCalled();
    expect(router.state.location.pathname).toBe("/chat");
  });

  it("stays when the save fails and the user declines to leave", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const router = routed("error", async () => false);
    await act(async () => screen.getByRole("link", { name: "对话" }).click());
    await vi.waitFor(() => expect(confirm).toHaveBeenCalledOnce());
    expect(router.state.location.pathname).toBe("/workflows");
  });

  it("leaves after a failed save when the user confirms", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const router = routed("dirty", async () => false);
    await act(async () => screen.getByRole("link", { name: "对话" }).click());
    expect(await screen.findByText("对话页")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/chat");
  });
});
