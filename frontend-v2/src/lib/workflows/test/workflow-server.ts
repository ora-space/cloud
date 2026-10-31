import type { AxiosAdapter, AxiosResponse, InternalAxiosRequestConfig } from "axios";
import { AxiosError } from "axios";
import { http } from "@/lib/api-client";
import { ContractError } from "../contracts";
import type { WorkflowContracts } from "../data/contracts-client";
import {
  LocalWorkflowBackend,
  type WorkflowBackendStorage,
} from "../data/local-workflow-backend";
import { addSampleWorkflows } from "../samples/seed";

/** Storage that lives as long as the test that made it. */
export function memoryWorkflowStorage(): WorkflowBackendStorage {
  let value: string | null = null;
  return {
    load: () => value,
    save: (serialized) => {
      value = serialized;
    },
  };
}

/** The local backend's rules behind the async contract the page calls, without HTTP. */
export function localWorkflowContracts(backend: LocalWorkflowBackend): WorkflowContracts {
  const call =
    <Request, Response>(operation: (request: Request) => Response) =>
    async (request: Request) =>
      operation(request);
  return {
    activate: call(backend.activate.bind(backend)),
    analyze: call(backend.analyze.bind(backend)),
    create: call(backend.create.bind(backend)),
    delete: call(backend.delete.bind(backend)),
    deleteSnapshot: call(backend.deleteSnapshot.bind(backend)),
    get: call(backend.get.bind(backend)),
    getDraft: call(backend.getDraft.bind(backend)),
    getVersion: call(backend.getVersion.bind(backend)),
    list: call((_request: Record<string, never>) => backend.list()),
    listVersions: call(backend.listVersions.bind(backend)),
    publish: call(backend.publish.bind(backend)),
    rollback: call(backend.rollback.bind(backend)),
    update: call(backend.update.bind(backend)),
    updateDraft: call(backend.updateDraft.bind(backend)),
  };
}

/** A fresh in-memory library holding the published samples, as a tenant that added them. */
export async function sampleWorkflowBackend(): Promise<LocalWorkflowBackend> {
  const backend = new LocalWorkflowBackend({ storage: memoryWorkflowStorage() });
  await addSampleWorkflows(localWorkflowContracts(backend));
  return backend;
}

/** What the fake workflow server saw; tests read it and can make the next request fail. */
export interface WorkflowServer {
  backend: LocalWorkflowBackend;
  /** Requests seen, as "METHOD /path". */
  seen: string[];
  /** The requests among them sent with `keepalive`, as "METHOD /path". */
  keptAlive: string[];
  /** Makes every request whose "METHOD /path" matches answer this instead. */
  refuse?: { match: RegExp; status: number; data: unknown };
}

/**
 * Serves v2-server's workflow definition routes for `tenantId` from `backend`, on top of whatever
 * fake the shared axios instance already answers with (the tenant list, chat routes). Responses
 * carry milliseconds as JSON numbers and refusals as `{code, params, requestId}`, as v2-server does.
 */
export function serveWorkflows(
  tenantId: string,
  backend: LocalWorkflowBackend,
): WorkflowServer {
  routeFetchToFakes();
  const server: WorkflowServer = { backend, seen: [], keptAlive: [] };
  const base = `/api/v2/tenants/${tenantId}/workflows`;
  const previous = http.defaults.adapter as AxiosAdapter | undefined;
  const adapter: AxiosAdapter = async (config) => {
    const url = config.url ?? "";
    if (url !== base && !url.startsWith(`${base}/`)) {
      if (typeof previous !== "function") throw new Error(`no fake for ${url}`);
      return previous(config);
    }
    const method = config.method?.toUpperCase() ?? "GET";
    server.seen.push(`${method} ${url}`);
    if ((config.fetchOptions as RequestInit | undefined)?.keepalive) {
      server.keptAlive.push(`${method} ${url}`);
    }
    const refused = server.refuse;
    const answer =
      refused && refused.match.test(`${method} ${url}`)
        ? { status: refused.status, data: refused.data }
        : route(backend, method, url.slice(base.length), body(config));
    const response: AxiosResponse = {
      status: answer.status,
      statusText: String(answer.status),
      data: answer.data,
      headers: {},
      config,
    };
    if (answer.status >= 400) {
      throw new AxiosError(`HTTP ${answer.status}`, String(answer.status), config, undefined, response);
    }
    return response;
  };
  http.defaults.adapter = adapter;
  return server;
}

let fetchRouted = false;

/**
 * The keepalive transport names axios's fetch adapter on the request, which would pass over the
 * fake installed as the default adapter; while a fake is installed, it answers those requests too.
 */
function routeFetchToFakes(): void {
  if (fetchRouted) return;
  fetchRouted = true;
  http.interceptors.request.use(
    (config) => {
      if (config.adapter === "fetch" && typeof http.defaults.adapter === "function") {
        config.adapter = http.defaults.adapter;
      }
      return config;
    },
    undefined,
    { synchronous: true },
  );
}

function body(config: InternalAxiosRequestConfig): Record<string, unknown> {
  if (typeof config.data === "string") return JSON.parse(config.data) as Record<string, unknown>;
  return (config.data ?? {}) as Record<string, unknown>;
}

function route(
  backend: LocalWorkflowBackend,
  method: string,
  path: string,
  request: Record<string, unknown>,
): { status: number; data: unknown } {
  const ok = (data: unknown, status = 200) => ({ status, data: wire(data) });
  try {
    if (path === "" && method === "GET") return ok(backend.list());
    if (path === "" && method === "POST")
      return ok(backend.create(request as { name: string; graph: string | null }), 201);
    if (path === "/analyze") return ok(backend.analyze(request as { graph: string }));
    const [, workflowId, rest = ""] = path.match(/^\/([^/]+)(\/.*)?$/) ?? [];
    if (!workflowId) return notFound();
    if (rest === "" && method === "GET") return ok(backend.get({ workflowId }));
    if (rest === "" && method === "PUT")
      return ok(backend.update({ workflowId, name: request.name as string }));
    if (rest === "" && method === "DELETE") return ok(backend.delete({ workflowId }));
    if (rest === "/draft" && method === "GET") return ok(backend.getDraft({ workflowId }));
    if (rest === "/draft" && method === "PUT")
      return ok(backend.updateDraft({ workflowId, graph: request.graph as string }));
    if (rest === "/publish")
      return ok(backend.publish({ workflowId, version: (request.version as string | null) ?? null }));
    if (rest === "/versions") return ok(backend.listVersions({ workflowId }));
    if (rest === "/activate")
      return ok(backend.activate({ workflowId, snapshotId: request.snapshotId as string }));
    if (rest === "/rollback")
      return ok(backend.rollback({ workflowId, snapshotId: request.snapshotId as string }));
    const version = rest.match(/^\/versions\/([^/]+)$/)?.[1];
    if (version && method === "GET")
      return ok(backend.getVersion({ workflowId, version: decodeURIComponent(version) }));
    if (version && method === "DELETE")
      return ok(backend.deleteSnapshot({ workflowId, version: decodeURIComponent(version) }));
    return notFound();
  } catch (error) {
    if (!(error instanceof ContractError)) throw error;
    const status = /not_found/.test(error.code)
      ? 404
      : /conflict|already_exists/.test(error.code)
        ? 409
        : 400;
    return { status, data: { code: error.code, params: error.params, requestId: "r" } };
  }
}

const notFound = () => ({
  status: 404,
  data: { code: "not_found", params: {}, requestId: "r" },
});

/** JSON as v2-server writes it: `bigint` milliseconds become numbers. */
function wire(data: unknown): unknown {
  return JSON.parse(
    JSON.stringify(data, (_key, value: unknown) =>
      typeof value === "bigint" ? Number(value) : value,
    ),
  );
}
