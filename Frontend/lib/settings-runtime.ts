import { z } from "zod";
import type { ExecutionSettings, ModelSettings, RunnerCapabilities } from "./api/generated/models/index";

const integer = (label: string, min: number, max: number) => z.string()
  .trim()
  .regex(/^\d+$/, `${label}请填写整数`)
  .transform(Number)
  .pipe(z.number().int().min(min, `${label}不能小于 ${min}`).max(max, `${label}不能大于 ${max}`));

const taskQuota = (label: string, max = Number.MAX_SAFE_INTEGER) => z.string()
  .trim()
  .regex(/^\d+$/, `${label}请填写 0 或正整数`)
  .transform(Number)
  .refine((value) => Number.isSafeInteger(value) && value >= 0 && value <= max, `${label}数值过大或无法准确保存`);
// Keep MiB-to-byte conversion within the backend's signed 64-bit integer range.
const maxMiB = 8796093022207;

export const executionFields = [
  { key: "maxConcurrent", label: "同时执行任务数", min: 1, max: 4, group: "运行限制" },
  { key: "timeoutSeconds", label: "单次最长运行时间（秒）", min: 0, max: 3600, group: "运行限制", hint: "0 为不限。" },
  { key: "memoryMb", label: "内存上限（MiB）", min: 64, max: 8192, group: "运行限制", resource: true },
  { key: "cpuMillis", label: "CPU 上限（毫核）", min: 100, max: 8000, group: "运行限制", resource: true, hint: "1000 毫核 = 1 个 CPU 核。" },
  { key: "pids", label: "进程数上限（PID）", min: 16, max: 512, group: "运行限制", resource: true },
  { key: "workspaceMb", label: "代码快照大小（MiB）", min: 0, group: "用量上限（0 为不限）" },
  { key: "maxFiles", label: "代码文件数量", min: 0, group: "用量上限（0 为不限）" },
  { key: "artifactMb", label: "执行产物大小（MiB）", min: 0, group: "用量上限（0 为不限）" },
  { key: "maxToolCalls", label: "工具调用次数", min: 0, group: "用量上限（0 为不限）" },
  { key: "maxTokens", label: "单次模型用量（Token）", min: 0, group: "用量上限（0 为不限）", hint: "0 会预留当天全部剩余额度，结束后按实际用量结算。" },
  { key: "jobRetryAttempts", label: "后台任务最大尝试次数", min: 1, max: 10, group: "重试策略", hint: "含首次执行。" },
  { key: "jobRetryBaseSeconds", label: "后台任务初始重试间隔（秒）", min: 1, max: 3600, group: "重试策略" },
  { key: "jobRetryMaxSeconds", label: "后台任务最大重试间隔（秒）", min: 1, max: 86400, group: "重试策略" },
  { key: "externalRetryAttempts", label: "平台发送自动重试次数", min: 0, max: 5, group: "重试策略", hint: "0 为不重试。" },
  { key: "externalRetryDelaySeconds", label: "平台发送重试间隔（秒）", min: 1, max: 86400, group: "重试策略" },
] as const;

type ExecutionNumberField = typeof executionFields[number]["key"];
export type ExecutionFormValues = Record<ExecutionNumberField, string> & {
  resourceLimits: "required" | "disabled";
  networkMode: "none" | "bridge";
  readOnlyRootfs: boolean;
  testWorkspaceWritable: boolean;
};

export function executionFormValues(settings: ExecutionSettings): ExecutionFormValues {
  return {
    resourceLimits: settings.resourceLimits === "disabled" ? "disabled" : "required",
    networkMode: settings.networkMode === "bridge" ? "bridge" : "none",
    readOnlyRootfs: settings.readOnlyRootfs ?? true,
    testWorkspaceWritable: settings.testWorkspaceWritable ?? false,
    maxConcurrent: String(settings.maxConcurrent ?? ""),
    timeoutSeconds: String(settings.timeoutSeconds ?? ""),
    memoryMb: String(settings.memoryMb ?? ""),
    cpuMillis: String(settings.cpuMillis ?? ""),
    pids: String(settings.pids ?? ""),
    workspaceMb: String(settings.workspaceMb ?? 0),
    maxFiles: String(settings.maxFiles ?? 0),
    maxToolCalls: String(settings.maxToolCalls ?? 0),
    maxTokens: String(settings.maxTokens ?? 0),
    artifactMb: String(settings.artifactMb ?? 0),
    jobRetryAttempts: String(settings.jobRetryAttempts ?? 5),
    jobRetryBaseSeconds: String(settings.jobRetryBaseSeconds ?? 2),
    jobRetryMaxSeconds: String(settings.jobRetryMaxSeconds ?? 64),
    externalRetryAttempts: String(settings.externalRetryAttempts ?? 0),
    externalRetryDelaySeconds: String(settings.externalRetryDelaySeconds ?? 60),
  };
}

const executionSchema = z.object({
  resourceLimits: z.enum(["required", "disabled"]),
  networkMode: z.enum(["none", "bridge"]),
  readOnlyRootfs: z.boolean(),
  testWorkspaceWritable: z.boolean(),
  maxConcurrent: integer("同时执行任务数", 1, 4),
  timeoutSeconds: integer("单次最长运行时间", 0, 3600),
  memoryMb: integer("内存上限", 64, 8192),
  cpuMillis: integer("CPU 上限", 100, 8000),
  pids: integer("进程数上限", 16, 512),
  workspaceMb: taskQuota("代码快照大小", maxMiB),
  maxFiles: taskQuota("代码文件数量"),
  maxToolCalls: taskQuota("工具调用次数"),
  maxTokens: taskQuota("单次模型用量"),
  artifactMb: taskQuota("执行产物大小", maxMiB),
  jobRetryAttempts: integer("后台任务最大尝试次数", 1, 10),
  jobRetryBaseSeconds: integer("后台任务初始重试间隔", 1, 3600),
  jobRetryMaxSeconds: integer("后台任务最大重试间隔", 1, 86400),
  externalRetryAttempts: integer("平台发送自动重试次数", 0, 5),
  externalRetryDelaySeconds: integer("平台发送重试间隔", 1, 86400),
}).refine((value) => value.resourceLimits !== "disabled" || value.maxConcurrent === 1, {
  path: ["maxConcurrent"], message: "关闭资源限制时，同时执行任务数必须为 1",
});

export function executionSettingsPayload(values: ExecutionFormValues, version: number) {
  return { ...executionSchema.parse(values), expectedVersion: version };
}

export function changeResourceLimits(values: ExecutionFormValues, mode: ExecutionFormValues["resourceLimits"]): ExecutionFormValues {
  return { ...values, resourceLimits: mode, maxConcurrent: mode === "disabled" ? "1" : values.maxConcurrent };
}

export type ModelFormValues = {
  enabled: boolean;
  baseUrl: string;
  modelName: string;
  apiKey: string;
  clearApiKey: boolean;
  allowHttp: boolean;
  jsonMode: "json_schema" | "json_object";
  maxTokensField: "max_tokens" | "max_completion_tokens";
  maxDailyTokens: string;
};

export function modelFormValues(settings: ModelSettings): ModelFormValues {
  return {
    enabled: settings.enabled ?? false,
    baseUrl: settings.baseUrl ?? "",
    modelName: settings.modelName ?? "",
    apiKey: "",
    clearApiKey: false,
    allowHttp: settings.allowHttp ?? false,
    jsonMode: settings.jsonMode === "json_object" ? "json_object" : "json_schema",
    maxTokensField: settings.maxTokensField === "max_completion_tokens" ? "max_completion_tokens" : "max_tokens",
    maxDailyTokens: String(settings.maxDailyTokens ?? ""),
  };
}

const modelSchema = z.object({
  enabled: z.boolean(),
  baseUrl: z.string().trim().max(2048, "API 地址不能超过 2048 个字符"),
  modelName: z.string().trim().max(200, "模型名称不能超过 200 个字符").regex(/^[^\r\n\0]*$/, "模型名称不能包含换行或空字符"),
  apiKey: z.string().trim().max(8192, "API 密钥不能超过 8192 个字符").regex(/^[^\r\n\0]*$/, "API 密钥不能包含换行或空字符"),
  clearApiKey: z.boolean(),
  allowHttp: z.boolean(),
  jsonMode: z.enum(["json_schema", "json_object"]),
  maxTokensField: z.enum(["max_tokens", "max_completion_tokens"]),
  maxDailyTokens: integer("每日模型用量上限", 1, 1000000000),
}).superRefine((value, ctx) => {
  if (value.enabled && !value.baseUrl) ctx.addIssue({ code: "custom", path: ["baseUrl"], message: "启用模型前，请填写 API 地址" });
  if (value.enabled && !value.modelName) ctx.addIssue({ code: "custom", path: ["modelName"], message: "启用模型前，请填写模型名称" });
  if (value.baseUrl) {
    try {
      const url = new URL(value.baseUrl);
      if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || value.baseUrl.includes("?") || value.baseUrl.includes("#"))
        throw new Error("invalid URL");
      if (url.protocol === "http:" && !value.allowHttp)
        ctx.addIssue({ code: "custom", path: ["baseUrl"], message: "请使用 HTTPS 地址；开发环境使用 HTTP 时需明确允许" });
    } catch {
      ctx.addIssue({ code: "custom", path: ["baseUrl"], message: "请填写有效的 HTTP(S) API 地址，不含账号、密码、查询参数或片段" });
    }
  }
  if (value.clearApiKey && value.apiKey)
    ctx.addIssue({ code: "custom", path: ["apiKey"], message: "清除密钥和填写新密钥不能同时选择" });
});

export function modelSettingsPayload(values: ModelFormValues, version: number, existing?: ModelSettings) {
  const { apiKey, ...settings } = modelSchema.parse(values);
  const normalizeAddress = (value: string) => value.trim().replace(/\/+$/, "");
  if (existing?.apiKeyConfigured && normalizeAddress(settings.baseUrl) !== normalizeAddress(existing.baseUrl ?? "") && !apiKey && !settings.clearApiKey)
    throw new Error("更换 API 地址时，请重新填写密钥或选择清除已有密钥");
  return { ...settings, ...(apiKey ? { apiKey } : {}), expectedVersion: version };
}

export function settingsValidationMessage(error: unknown): string {
  return error instanceof z.ZodError ? error.issues[0]?.message ?? "请检查填写内容" : error instanceof Error ? error.message : "请检查填写内容";
}

export function runtimeHostKnown(runtime?: RunnerCapabilities | null): boolean {
  if (runtime?.machines) return runtime.machines.length > 0 && runtime.machines.every(runtimeHostKnown);
  return runtime?.reachable === true && runtime.errorEscaped !== "runner runtime unavailable";
}

export function cgroupRuntimeLabel(runtime?: RunnerCapabilities | null): string {
  if (!runtime?.cgroupVersion) return "资源限制状态未知";
  const driver = runtime.cgroupDriver === "none"
    ? "未启用资源控制驱动"
    : runtime.cgroupDriver ? `${runtime.cgroupDriver} 驱动` : "驱动信息未提供";
  return `cgroup v${runtime.cgroupVersion} · ${driver}`;
}

export function executionRuntimeIssue(runtime?: RunnerCapabilities | null): { message: string; modelLink?: boolean } | undefined {
  if (!runtime?.reachable) return undefined;
  if (!runtimeHostKnown(runtime)) return { message: "容器环境检查未通过，请检查 Rootless Docker、运行目录和磁盘空间。" };
  if (runtime.rootless === false || runtime.filesystemReady === false || runtime.errorEscaped === "runner isolation preflight unavailable")
    return { message: "隔离检查未通过，请检查 Rootless Docker、数据目录和磁盘空间。" };
  if (runtime.settingsConfigured === false || runtime.errorEscaped === "runner settings unavailable")
    return { message: "执行配置未同步，请保存任务执行设置，并检查机器能否访问 API。" };
  if (runtime.resourceLimits === "required" && runtime.supportsResourceLimits === false)
    return { message: "当前机器不支持资源限制，请检查机器配置。仅可信开发环境可关闭限制。" };
  if (runtime.modelConfigured === false || runtime.errorEscaped === "model settings are missing, disabled or invalid")
    return { message: "模型未就绪，请到“模型接口”启用并保存。", modelLink: true };
  if (runtime.errorEscaped || runtime.ready === false)
    return { message: "运行机器不可用，请检查 Runner 服务日志后刷新。" };
  return undefined;
}
