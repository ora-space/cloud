import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  WORKFLOW_DRAFT_AUTOSAVE_MS,
  useWorkflowDraftAutosave,
} from "./use-workflow-draft-autosave";

describe("useWorkflowDraftAutosave", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("debounces persistable edits into one save", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
      result.current.markDirty();
      result.current.markDirty();
    });
    expect(result.current.status).toBe("dirty");
    expect(save).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });

    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("clean");
  });

  it("saves pending edits when the page goes away, and only once", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    // A clean draft has nothing to write on reload.
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(save).not.toHaveBeenCalled();

    act(() => {
      result.current.markDirty();
    });
    await act(async () => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(save).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
  });

  it("hands unsaved and in-flight edits to leave on pagehide instead of starting a write", async () => {
    let finish: (value: "saved") => void = () => undefined;
    const save = vi.fn(() => new Promise<"saved">((resolve) => (finish = resolve)));
    const leave = vi.fn();
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save, leave }),
    );

    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(leave).not.toHaveBeenCalled();

    act(() => {
      result.current.markDirty();
    });
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(leave).toHaveBeenCalledTimes(1);
    expect(save).not.toHaveBeenCalled();

    // A write already in flight may not land before the page unloads, so it is kept too.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    expect(leave).toHaveBeenCalledTimes(2);
    await act(async () => {
      finish("saved");
    });
    expect(result.current.status).toBe("clean");
  });

  it("flush writes immediately and cancels the pending timer", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    await act(async () => {
      await result.current.flush();
    });

    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("clean");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
  });

  it("reschedules when a save returns stale because edits continued", async () => {
    const save = vi
      .fn()
      .mockResolvedValueOnce("stale" as const)
      .mockResolvedValueOnce("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("dirty");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("clean");
  });

  it("flush can force a write even when the draft was never marked dirty", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    await act(async () => {
      await result.current.flush({ force: true });
    });

    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("clean");
  });

  it("keeps dirty when a save is skipped so later flush can still persist", async () => {
    const save = vi.fn().mockResolvedValue("skipped" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });

    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("dirty");

    save.mockResolvedValueOnce("saved" as const);
    await act(async () => {
      await result.current.flush();
    });
    expect(save).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("clean");
  });

  it("resumes a pending debounce after autosave is re-enabled", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result, rerender } = renderHook(
      ({ enabled }) => useWorkflowDraftAutosave({ enabled, save }),
      { initialProps: { enabled: true } },
    );

    act(() => {
      result.current.markDirty();
    });
    rerender({ enabled: false });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).not.toHaveBeenCalled();
    expect(result.current.status).toBe("dirty");

    rerender({ enabled: true });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("clean");
  });

  it("flushes dirty edits on unmount instead of dropping them", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result, unmount } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    unmount();

    expect(save).toHaveBeenCalledTimes(1);
  });

  it("hands unsaved and in-flight edits to leave on unmount, as on a route change", async () => {
    let finish: (value: "saved") => void = () => undefined;
    const save = vi.fn(
      () => new Promise<"saved">((resolve) => (finish = resolve)),
    );
    const leave = vi.fn();
    const first = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save, leave }),
    );
    act(() => {
      first.result.current.markDirty();
    });
    first.unmount();
    expect(leave).toHaveBeenCalledTimes(1);
    expect(save).not.toHaveBeenCalled();

    const second = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save, leave }),
    );
    act(() => {
      second.result.current.markDirty();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).toHaveBeenCalledTimes(1);
    second.unmount();
    expect(leave).toHaveBeenCalledTimes(2);
    finish("saved");

    const clean = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save, leave }),
    );
    clean.unmount();
    expect(leave).toHaveBeenCalledTimes(2);
  });

  it("flush drains stale attempts before reporting success", async () => {
    const save = vi
      .fn()
      .mockResolvedValueOnce("stale" as const)
      .mockResolvedValueOnce("saved" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    let saved = false;
    await act(async () => {
      saved = await result.current.flush();
    });

    expect(saved).toBe(true);
    expect(save).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("clean");
  });

  it("flush returns false when the write fails so navigation can abort", async () => {
    const save = vi.fn().mockResolvedValue("failed" as const);
    const { result } = renderHook(() =>
      useWorkflowDraftAutosave({ enabled: true, save }),
    );

    act(() => {
      result.current.markDirty();
    });
    let saved = true;
    await act(async () => {
      saved = await result.current.flush();
    });

    expect(saved).toBe(false);
    expect(result.current.status).toBe("error");
  });

  it("ignores edits while disabled and cancel drops pending dirty state", async () => {
    const save = vi.fn().mockResolvedValue("saved" as const);
    const { result, rerender } = renderHook(
      ({ enabled }) => useWorkflowDraftAutosave({ enabled, save }),
      { initialProps: { enabled: false } },
    );

    act(() => {
      result.current.markDirty();
    });
    expect(result.current.status).toBe("clean");

    rerender({ enabled: true });
    act(() => {
      result.current.markDirty();
    });
    expect(result.current.status).toBe("dirty");

    act(() => {
      result.current.cancel();
    });
    expect(result.current.status).toBe("clean");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(WORKFLOW_DRAFT_AUTOSAVE_MS);
    });
    expect(save).not.toHaveBeenCalled();
  });
});
