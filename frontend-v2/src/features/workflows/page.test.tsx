import "@/lib/workflows/test/dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { TooltipProvider } from "@/components/ui/tooltip";
import { defaultWorkflowSidebar, useUiStore } from "@/lib/workflows/host/ui-store";
import { LocalWorkflowBackend, browserWorkflowStorage } from "@/lib/workflows/data/local-workflow-backend";
import {
  memoryWorkflowStorage,
  sampleWorkflowBackend,
  serveWorkflows,
  type WorkflowServer,
} from "@/lib/workflows/test/workflow-server";
import { demoUser, fakeBackend } from "@/test/backend";
import Page from "./page";

const tenantId = "00000000-0000-4000-8000-0000000000aa";

/** The signed-in user's one tenant, whose workflows `server` holds. */
function serve(backend: LocalWorkflowBackend): WorkflowServer {
  fakeBackend((config) => {
    if (config.url === "/api/v1/me") return { status: 200, data: demoUser };
    if (config.url === "/api/v1/me/tenants")
      return {
        status: 200,
        data: { items: [{ id: tenantId, name: "演示团队", status: "active", role: "owner" }], nextCursor: "" },
      };
    return undefined;
  });
  return serveWorkflows(tenantId, backend);
}

/** Renders the page and waits until it has read the tenant and shows the library. */
async function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const rendered = render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <Page />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await screen.findByRole("complementary", { name: "工作流列表" });
  return rendered;
}

const library = () => screen.getByRole("complementary", { name: "工作流列表" });

/** Creates a workflow through desktop's library menu and name dialog. */
async function createWorkflow(user: ReturnType<typeof userEvent.setup>, name: string) {
  const addMenu = await within(library()).findByRole("button", {
    name: "新建或导入工作流",
  });
  // The library enables its menu once the editor registers its actions.
  await waitFor(() => expect(addMenu).toBeEnabled());
  // Base UI opens the menu on a single click event, as desktop's tests drive it.
  fireEvent.click(addMenu);
  await user.click(await screen.findByRole("menuitem", { name: /新建工作流/ }));
  const dialog = await screen.findByRole("alertdialog", { name: "新建工作流" });
  await user.type(within(dialog).getByLabelText("工作流名称"), name);
  await user.click(within(dialog).getByRole("button", { name: "新建工作流" }));
}

describe("workflow page", () => {
  let server: WorkflowServer;
  beforeEach(async () => {
    window.localStorage.clear();
    server = serve(await sampleWorkflowBackend());
    useUiStore.setState({
      sidebarCollapsed: defaultWorkflowSidebar.collapsed,
      sidebarWidth: defaultWorkflowSidebar.width,
    });
  });

  it("opens on the tenant's workflows from the server", async () => {
    await renderPage();

    for (const name of ["示例：代码审查", "示例：逐段翻译", "循环综合测试：分支与退出"]) {
      expect(await within(library()).findByRole("button", { name })).toBeInTheDocument();
    }
    expect(await screen.findByDisplayValue("示例：代码审查")).toBeInTheDocument();
    expect(await screen.findByText("生效中 · v1")).toBeInTheDocument();
    expect(server.seen).toContain(`GET /api/v2/tenants/${tenantId}/workflows`);
  });

  it("adds the samples only when asked", async () => {
    const user = userEvent.setup();
    server = serve(new LocalWorkflowBackend({ storage: memoryWorkflowStorage() }));
    await renderPage();
    const add = await screen.findByRole("button", { name: "添加样例工作流" });
    expect(within(library()).queryByRole("button", { name: "示例：代码审查" })).not.toBeInTheDocument();

    await user.click(add);

    expect(await within(library()).findByRole("button", { name: "示例：代码审查" })).toBeInTheDocument();
    expect(await screen.findByText("添加了 5 个样例")).toBeInTheDocument();
    expect(server.backend.list().workflows).toHaveLength(5);
  });

  it("says so when the server refuses to publish, and keeps the draft unpublished", async () => {
    const user = userEvent.setup();
    await renderPage();
    await createWorkflow(user, "待发布");
    await screen.findByDisplayValue("待发布");
    server.refuse = {
      match: /POST .*\/publish$/,
      status: 503,
      data: { code: "workflow_domain_unavailable", params: {}, requestId: "r" },
    };

    await user.click(await screen.findByRole("button", { name: "发布" }));
    const dialog = await screen.findByRole("alertdialog", { name: "发布工作流" });
    await user.type(within(dialog).getByRole("textbox"), "v1");
    await user.click(within(dialog).getByRole("button", { name: "发布" }));

    expect(
      await screen.findByText("服务端暂时不能检查和发布工作流，草稿已保存。"),
    ).toBeInTheDocument();
    expect(screen.getByText("暂未发布")).toBeInTheDocument();
  });

  it("keeps the edits a reload interrupts and writes them before the editor reads the library", async () => {
    const user = userEvent.setup();
    const first = await renderPage();
    await createWorkflow(user, "离页");
    const canvas = await screen.findByLabelText("工作流画布");
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "1"));
    await user.click(
      within(screen.getByRole("toolbar", { name: "添加工作流节点" })).getByRole("button", { name: /^Agent$/ }),
    );
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "2"));
    expect(screen.getByText("未保存")).toBeInTheDocument();

    // The page goes away inside the quiet period, and no request it starts gets through.
    server.refuse = {
      match: /PUT .*\/draft$/,
      status: 503,
      data: { code: "workflow_store_unavailable", params: {}, requestId: "r" },
    };
    fireEvent(window, new Event("pagehide"));
    first.unmount();
    await waitFor(() => expect(server.seen.at(-1)).toMatch(/PUT .*\/draft$/));
    server.refuse = undefined;
    const id = server.backend.list().workflows.find((w) => w.name === "离页")!.id;
    const stored = () =>
      (JSON.parse(server.backend.getDraft({ workflowId: id }).snapshot.graph) as { nodes: unknown[] })
        .nodes.length;
    expect(stored()).toBe(1);

    await renderPage();
    await user.click(await within(library()).findByRole("button", { name: "离页" }));
    await waitFor(() =>
      expect(screen.getByLabelText("工作流画布")).toHaveAttribute("data-workflow-node-count", "2"),
    );
    expect(stored()).toBe(2);
  });

  it("keeps the edits a route change leaves when their save is refused, and writes them on return", async () => {
    const user = userEvent.setup();
    const first = await renderPage();
    await createWorkflow(user, "切页");
    const canvas = await screen.findByLabelText("工作流画布");
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "1"));
    await user.click(
      within(screen.getByRole("toolbar", { name: "添加工作流节点" })).getByRole("button", { name: /^Agent$/ }),
    );
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "2"));

    // Navigating to another page unmounts the editor: no pagehide, and the write is refused.
    server.refuse = {
      match: /PUT .*\/draft$/,
      status: 503,
      data: { code: "workflow_store_unavailable", params: {}, requestId: "r" },
    };
    first.unmount();
    await waitFor(() => expect(server.keptAlive.at(-1)).toMatch(/PUT .*\/draft$/));
    server.refuse = undefined;
    const id = server.backend.list().workflows.find((w) => w.name === "切页")!.id;
    const stored = () =>
      (JSON.parse(server.backend.getDraft({ workflowId: id }).snapshot.graph) as { nodes: unknown[] })
        .nodes.length;
    expect(stored()).toBe(1);

    await renderPage();
    await user.click(await within(library()).findByRole("button", { name: "切页" }));
    await waitFor(() =>
      expect(screen.getByLabelText("工作流画布")).toHaveAttribute("data-workflow-node-count", "2"),
    );
    expect(stored()).toBe(2);
  });

  it("asks before writing kept edits over a teammate's later change", async () => {
    const user = userEvent.setup();
    const first = await renderPage();
    await createWorkflow(user, "共用");
    const canvas = await screen.findByLabelText("工作流画布");
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "1"));
    await user.click(
      within(screen.getByRole("toolbar", { name: "添加工作流节点" })).getByRole("button", { name: /^Agent$/ }),
    );
    await waitFor(() => expect(canvas).toHaveAttribute("data-workflow-node-count", "2"));

    // The page goes away before its last edit reaches the server; a teammate then edits the draft.
    server.refuse = {
      match: /PUT .*\/draft$/,
      status: 503,
      data: { code: "workflow_store_unavailable", params: {}, requestId: "r" },
    };
    fireEvent(window, new Event("pagehide"));
    first.unmount();
    await waitFor(() => expect(server.keptAlive.length).toBeGreaterThan(0));
    server.refuse = undefined;
    const id = server.backend.list().workflows.find((w) => w.name === "共用")!.id;
    const graph = JSON.parse(server.backend.getDraft({ workflowId: id }).snapshot.graph) as {
      nodes: unknown[];
      description?: string;
    };
    expect(graph.nodes).toHaveLength(1);
    const theirs = JSON.stringify({ ...graph, description: "队友改的" });
    server.backend.updateDraft({ workflowId: id, graph: theirs });

    render(
      <QueryClientProvider client={new QueryClient()}>
        <TooltipProvider>
          <Page />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    const conflict = await screen.findByRole("region", { name: "「共用」离开前的修改" });
    expect(server.backend.getDraft({ workflowId: id }).snapshot.graph).toBe(theirs);

    await user.click(within(conflict).getByRole("button", { name: "另存为新工作流" }));

    expect(await within(library()).findByRole("button", { name: "共用（离开前的修改）" })).toBeInTheDocument();
    expect(server.backend.getDraft({ workflowId: id }).snapshot.graph).toBe(theirs);
    const copy = server.backend.list().workflows.find((w) => w.name === "共用（离开前的修改）")!;
    expect(
      (JSON.parse(server.backend.getDraft({ workflowId: copy.id }).snapshot.graph) as { nodes: unknown[] }).nodes,
    ).toHaveLength(2);
  });

  it("offers once to import the workflows this browser kept", async () => {
    const user = userEvent.setup();
    new LocalWorkflowBackend({ storage: browserWorkflowStorage }).create({ name: "本机旧流程", graph: null });
    const first = await renderPage();
    const notice = await screen.findByRole("region", { name: "本机旧工作流" });
    expect(notice).toHaveTextContent("还有 1 个旧的本机工作流");

    await user.click(within(notice).getByRole("button", { name: "导入" }));

    expect(await within(library()).findByRole("button", { name: "本机旧流程" })).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "本机旧工作流" })).not.toBeInTheDocument();
    first.unmount();
    await renderPage();
    await within(library()).findByRole("button", { name: "本机旧流程" });
    expect(screen.queryByRole("region", { name: "本机旧工作流" })).not.toBeInTheDocument();
  });

  it("resizes the library from its edge and folds it past the minimum, like the chat history", async () => {
    const user = userEvent.setup();
    await renderPage();
    const edge = screen.getByRole("separator", { name: "调整工作流列表宽度" });

    edge.focus();
    await user.keyboard("{ArrowRight}");
    expect(edge).toHaveAttribute("aria-valuenow", "304");
    expect(library()).toHaveStyle({ width: "304px" });

    useUiStore.setState({ sidebarWidth: 200 });
    await user.keyboard("{ArrowLeft}");
    expect(screen.queryByRole("complementary", { name: "工作流列表" })).not.toBeInTheDocument();

    // Folded, the editor header offers only the button that shows the library again.
    expect(screen.queryByRole("button", { name: "返回" })).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "展开侧边栏" }));
    expect(library()).toHaveStyle({ width: "200px" });
  });

  it("lets every toast be dismissed", async () => {
    const user = userEvent.setup();
    await renderPage();
    await screen.findByDisplayValue("示例：代码审查");

    await user.click(await screen.findByRole("button", { name: "整理节点" }));
    const toast = await screen.findByText("已整理工作流节点");
    const item = toast.closest("[data-sonner-toast]") as HTMLElement;
    await user.click(within(item).getByRole("button", { name: "Close toast" }));
    await waitFor(() => expect(toast).not.toBeInTheDocument());
  });

  it("creates a workflow from the library and opens its canvas at the Start node", async () => {
    const user = userEvent.setup();
    await renderPage();

    await createWorkflow(user, "审查流程");

    expect(
      await within(library()).findByRole("button", { name: "审查流程" }),
    ).toBeInTheDocument();
    expect(await screen.findByDisplayValue("审查流程")).toBeInTheDocument();
    const canvas = await screen.findByLabelText("工作流画布");
    expect((await within(canvas).findAllByText("开始")).length).toBeGreaterThan(0);
  });

  it("keeps workflows across a reload of the page", async () => {
    const user = userEvent.setup();
    const first = await renderPage();
    await createWorkflow(user, "留存");
    await within(library()).findByRole("button", { name: "留存" });
    first.unmount();

    await renderPage();
    expect(
      await within(library()).findByRole("button", { name: "留存" }),
    ).toBeInTheDocument();
  });
});
