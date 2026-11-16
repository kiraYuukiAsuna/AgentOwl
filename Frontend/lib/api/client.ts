"use client";
import useSWR, { mutate } from "swr";
import { AnonymousAuthenticationProvider } from "@microsoft/kiota-abstractions";
import {
  FetchRequestAdapter,
  HttpClient,
} from "@microsoft/kiota-http-fetchlibrary";
import {
  createAgentOwlClient,
  type AgentOwlClient,
} from "./generated/agentOwlClient";
import type {
  Action,
  CaseEscaped,
  Channel,
  Confirmation,
  Connector,
  Decision,
  ExecutionSettingsUpdate,
  Feedback,
  IdentityBinding,
  JobRetry,
  JobIgnore,
  Knowledge,
  KnowledgeSource,
  Login,
  ModelSettingsUpdate,
  OwnerRoute,
  Policy,
  Relation,
  ReplyRequest,
  Repository,
  RuntimeMachine,
  User,
  VersionRequest,
} from "./generated/models";
import type { JobsRequestBuilderGetQueryParameters as JobsQuery } from "./generated/api/v1/jobs";
import type { CasesRequestBuilderGetQueryParameters as ListQuery } from "./generated/api/v1/cases";
import type { SettingsResource } from "@/lib/settings";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}
let sdk: AgentOwlClient | undefined;
let csrfToken = "";
const uncertainRequests = new Map<string, string>();

const settingResourcePaths: Record<SettingsResource, string> = {
  connectors: "connectors",
  channels: "channels",
  repositories: "repositories",
  runtimeMachines: "runtime-machines",
  policies: "policies",
  principals: "principals",
  identityBindings: "identity-bindings",
  ownerRoutes: "owner-routes",
  knowledgeSources: "knowledge-sources",
};

async function rawMutation(path: string, method: string, body: Record<string, unknown>) {
  const text = JSON.stringify(body);
  const headers = new Headers({ "Content-Type": "application/json" });
  if (csrfToken) headers.set("X-CSRF-Token", csrfToken);
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(`${method}:${path}:${text}`),
  );
  const fingerprint = Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
  const key = uncertainRequests.get(fingerprint) ?? crypto.randomUUID();
  uncertainRequests.set(fingerprint, key);
  headers.set("Idempotency-Key", key);
  let response: Response;
  try {
    response = await fetch(`${window.location.origin}${path}`, {
      method,
      headers,
      body: text,
      credentials: "same-origin",
      cache: "no-store",
    });
  } catch {
    throw new ApiError(
      0,
      "连接中断，操作结果未知。请保持内容不变后重试。",
    );
  }
  if (response.ok || response.status < 500) uncertainRequests.delete(fingerprint);
  if (!response.ok) {
    const problem = (await response
      .clone()
      .json()
      .catch(() => ({}))) as {
      detail?: string;
      title?: string;
      code?: string;
    };
    if (response.status === 401 && path !== "/api/v1/me" && path !== "/auth/login")
      void mutate("me");
    if (response.status === 409)
      void mutate((key) => typeof key !== "string" || key !== "me");
    const message = problem.detail || problem.title || `请求失败（${response.status}）`;
    throw new ApiError(
      response.status,
      response.status === 409 ? `${message}。请刷新后重试。` : message,
      problem.code,
    );
  }
  return response.json().catch(() => undefined) as Promise<unknown>;
}

function client() {
  if (sdk) return sdk;
  const http = new HttpClient(async (url, init) => {
    const headers = new Headers(init.headers);
    const method = init.method?.toUpperCase() ?? "GET";
    const mutation = !["GET", "HEAD", "OPTIONS"].includes(method);
    const path = new URL(url).pathname;
    let fingerprint: string | undefined;
    if (mutation) {
      if (csrfToken) headers.set("X-CSRF-Token", csrfToken);
      // Keep the same key when a network interruption leaves the result unknown.
      const digest = await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(
          `${method}:${path}:${String(init.body ?? "")}`,
        ),
      );
      fingerprint = Array.from(new Uint8Array(digest))
        .map((byte) => byte.toString(16).padStart(2, "0"))
        .join("");
      const key = uncertainRequests.get(fingerprint) ?? crypto.randomUUID();
      uncertainRequests.set(fingerprint, key);
      headers.set("Idempotency-Key", key);
    }
    let response: Response;
    try {
      response = await fetch(url, {
        ...init,
        headers,
        credentials: "same-origin",
        cache: "no-store",
      });
    } catch {
      throw new ApiError(
        0,
        "连接中断，操作结果未知。请保持内容不变后重试。",
      );
    }
    if (fingerprint && (response.ok || response.status < 500))
      uncertainRequests.delete(fingerprint);
    if (!response.ok) {
      const problem = (await response
        .clone()
        .json()
        .catch(() => ({}))) as {
        detail?: string;
        title?: string;
        code?: string;
      };
      if (
        response.status === 401 &&
        path !== "/api/v1/me" &&
        path !== "/auth/login"
      )
        void mutate("me");
      if (response.status === 409)
        void mutate((key) => typeof key !== "string" || key !== "me");
      const message =
        problem.detail || problem.title || `请求失败（${response.status}）`;
      throw new ApiError(
        response.status,
        response.status === 409
          ? `${message}。请刷新后重试。`
          : message,
        problem.code,
      );
    }
    return response;
  });
  const adapter = new FetchRequestAdapter(
    new AnonymousAuthenticationProvider(),
    undefined,
    undefined,
    http,
  );
  adapter.baseUrl = window.location.origin;
  sdk = createAgentOwlClient(adapter);
  return sdk;
}

function query(queryParameters: ListQuery = {}) {
  return { queryParameters };
}
const resource = () => client().api.v1;

export const api = {
  authConfig: () => client().auth.config.get(),
  async me() {
    const user = await resource().me.get();
    csrfToken = user?.csrfToken ?? "";
    return user;
  },
  async login(body: Login) {
    const user = await client().auth.login.post(body);
    csrfToken = user?.csrfToken ?? "";
    uncertainRequests.clear();
    await mutate(() => true, undefined, { revalidate: false });
    return user;
  },
  async logout() {
    await client().auth.logout.post();
    csrfToken = "";
    uncertainRequests.clear();
    await mutate(() => true, undefined, { revalidate: false });
  },
  updateProfile: (body: Record<string, unknown>) =>
    rawMutation("/api/v1/me", "PATCH", {
      expected_version: body.expectedVersion ?? body.expected_version ?? 0,
      name: body.name,
      current_password: body.currentPassword ?? body.current_password ?? "",
      new_password: body.newPassword ?? body.new_password ?? "",
    }),
  dashboard: () => resource().dashboard.get(),
  cases: (q?: ListQuery) => resource().cases.get(query(q)),
  case: (id: string) => resource().cases.byId(id).get(),
  createCase: (body: CaseEscaped) => resource().cases.post(body),
  updateCase: (id: string, body: CaseEscaped) =>
    resource().cases.byId(id).patch(body),
  caseAction: (id: string, body: Action) =>
    resource().cases.byId(id).actions.post(body),
  caseFeedback: (id: string, body: Feedback) =>
    resource().cases.byId(id).feedback.post(body),
  caseAttachmentContent: (id: string, index: number, version: number) =>
    resource()
      .cases.byId(id)
      .attachments.byIndex(index)
      .content.get({
        queryParameters: { expectedVersion: version },
      }),
  relateCase: (id: string, body: Relation) =>
    resource().cases.byId(id).relations.post(body),
  reply: (id: string, body: ReplyRequest) =>
    resource().cases.byId(id).replyDrafts.post(body),
  submitReply: (id: string, body: VersionRequest) =>
    resource().replies.byId(id).submit.post(body),
  confirmDevelopment: (id: string, body: Confirmation) =>
    resource().cases.byId(id).developmentConfirmations.post(body),
  approvals: (q?: ListQuery) => resource().approvals.get(query(q)),
  approval: (id: string) => resource().approvals.byId(id).get(),
  decide: (id: string, body: Decision) =>
    resource().approvals.byId(id).decisions.post(body),
  tasks: (q?: ListQuery) => resource().developmentTasks.get(query(q)),
  task: (id: string) => resource().developmentTasks.byId(id).get(),
  taskAction: (id: string, body: Action) =>
    resource().developmentTasks.byId(id).actions.post(body),
  reviseTask: (id: string, body: Confirmation) =>
    resource().developmentTasks.byId(id).revisions.post(body),
  runEvents: (id: string, q?: ListQuery) =>
    resource().runAttempts.byId(id).events.get(query(q)),
  artifactContent: (id: string) => resource().artifacts.byId(id).content.get(),
  knowledge: (q?: ListQuery) => resource().knowledge.get(query(q)),
  knowledgeItem: (id: string) => resource().knowledge.byId(id).get(),
  createKnowledge: (body: Knowledge) => resource().knowledge.post(body),
  newKnowledgeVersion: (id: string, body: Knowledge) =>
    resource().knowledge.byId(id).versions.post(body),
  knowledgeVersions: (id: string) =>
    resource().knowledge.byId(id).versions.get(),
  submitKnowledge: (id: string, body: VersionRequest) =>
    resource().knowledge.byId(id).submit.post(body),
  withdrawKnowledge: (id: string, body: VersionRequest) =>
    resource().knowledge.byId(id).withdraw.post(body),
  principals: (q?: ListQuery) => resource().principals.get(query(q)),
  connectors: (q?: ListQuery) => resource().connectors.get(query(q)),
  channels: (q?: ListQuery) => resource().channels.get(query(q)),
  repositories: (q?: ListQuery) => resource().repositories.get(query(q)),
  runtimeMachines: (q?: ListQuery) => resource().runtimeMachines.get(query(q)),
  audit: (q?: ListQuery) => resource().auditEvents.get(query(q)),
  jobs: (q?: JobsQuery) => resource().jobs.get({ queryParameters: q }),
  job: (id: string) => resource().jobs.byId(id).get(),
  ignoreJob: (id: string, body: JobIgnore) =>
    resource().jobs.byId(id).ignore.post(body),
  retryJob: (id: string, body: JobRetry) =>
    resource().jobs.byId(id).retry.post(body),
  externalOperations: (q?: ListQuery) =>
    resource().externalOperations.get(query(q)),
  reconcileOperation: (id: string, body: VersionRequest) =>
    resource().externalOperations.byId(id).reconcile.post(body),
  modelSettings: () => resource().settings.model.get(),
  saveModelSettings: (body: ModelSettingsUpdate) =>
    resource().settings.model.put(body),
  executionSettings: () => resource().settings.execution.get(),
  saveExecutionSettings: (body: ExecutionSettingsUpdate) =>
    resource().settings.execution.put(body),
  listSettings: (name: SettingsResource, q?: ListQuery) =>
    resource()[name].get(query(q)),
  createSetting(name: SettingsResource, body: Record<string, unknown>) {
    switch (name) {
      case "connectors":
        return resource().connectors.post(body as Connector);
      case "channels":
        return resource().channels.post(body as Channel);
      case "repositories":
        return resource().repositories.post(body as Repository);
      case "runtimeMachines":
        return resource().runtimeMachines.post(body as RuntimeMachine);
      case "policies":
        return resource().policies.post(body as Policy);
      case "principals":
        return resource().principals.post(body as User);
      case "identityBindings":
        return resource().identityBindings.post(body as IdentityBinding);
      case "ownerRoutes":
        return resource().ownerRoutes.post(body as OwnerRoute);
      case "knowledgeSources":
        return resource().knowledgeSources.post(body as KnowledgeSource);
    }
  },
  updateSetting(
    name: SettingsResource,
    id: string,
    body: Record<string, unknown>,
  ) {
    switch (name) {
      case "connectors":
        return resource()
          .connectors.byId(id)
          .patch(body as Connector);
      case "channels":
        return resource()
          .channels.byId(id)
          .patch(body as Channel);
      case "repositories":
        return resource()
          .repositories.byId(id)
          .patch(body as Repository);
      case "runtimeMachines":
        return resource()
          .runtimeMachines.byId(id)
          .patch(body as RuntimeMachine);
      case "policies":
        return resource()
          .policies.byId(id)
          .patch(body as Policy);
      case "principals":
        return resource()
          .principals.byId(id)
          .patch(body as User);
      case "identityBindings":
        return resource()
          .identityBindings.byId(id)
          .patch(body as IdentityBinding);
      case "ownerRoutes":
        return resource()
          .ownerRoutes.byId(id)
          .patch(body as OwnerRoute);
      case "knowledgeSources":
        return resource()
          .knowledgeSources.byId(id)
          .patch(body as KnowledgeSource);
    }
  },
  deleteSetting(name: SettingsResource, id: string, body: Record<string, unknown>) {
    return rawMutation(`/api/v1/${settingResourcePaths[name]}/${id}`, "DELETE", {
      expected_version: body.expectedVersion ?? body.expected_version ?? 0,
      force: Boolean(body.force),
    });
  },
  settingAction(
    name: SettingsResource,
    id: string,
    action: string,
    body: VersionRequest,
  ) {
    if (name === "connectors" && action === "verify")
      return resource().connectors.byId(id).verify.post(body);
    if (name === "channels" && action === "sync")
      return resource().channels.byId(id).sync.post(body);
    if (name === "knowledgeSources" && action === "sync")
      return resource().knowledgeSources.byId(id).sync.post(body);
    return Promise.reject(new Error("当前内容不支持此操作"));
  },
};
export function useMe() {
  return useSWR<User | undefined, ApiError>("me", api.me, {
    revalidateOnFocus: true,
    refreshInterval: 60000,
    shouldRetryOnError: false,
  });
}

export async function runtimeRead<T>(path: string): Promise<T> {
  const response = await fetch(path, { credentials: "same-origin", cache: "no-store" });
  const body = await response.json();
  if (!response.ok) throw new ApiError(response.status, body.detail || body.title || "加载失败", body.code);
  return body as T;
}
export const runtimeWrite = rawMutation;
