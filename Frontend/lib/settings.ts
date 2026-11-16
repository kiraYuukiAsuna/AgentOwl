import { z } from "zod";

export type SettingsResource =
  | "connectors"
  | "channels"
  | "repositories"
  | "runtimeMachines"
  | "policies"
  | "principals"
  | "identityBindings"
  | "ownerRoutes"
  | "knowledgeSources";
export type SettingField = {
  key: string;
  label: string;
  kind?:
  | "text"
  | "password"
  | "textarea"
  | "number"
  | "boolean"
  | "select"
  | "lines"
  | "json";
  required?: boolean;
  placeholder?: string;
  hint?: string;
  options?: { value: string; label: string }[];
  lookup?: "principals" | "connectors" | "channels" | "repositories" | "runtimeMachines";
  default?: string | boolean | number;
  group?: string;
  advanced?: boolean;
  visibleWhen?: { key: string; values: string[] }[];
};
export type SettingSection = {
  title: string;
  resource: SettingsResource;
  fields: SettingField[];
  columns: { key: string; label: string }[];
  action?: "verify" | "sync";
};
const options = (items: string[]) =>
  items.map((value) => ({ value, label: value }));
const visibility = {
  key: "visibility",
  label: "可见范围",
  kind: "select" as const,
  options: [
    { value: "internal", label: "团队内部" },
    { value: "public", label: "公开" },
  ],
  default: "internal",
};
const enabled = {
  key: "enabled",
  label: "启用",
  kind: "boolean" as const,
  default: true,
};
const token = {
  key: "token",
  label: "访问令牌（Token）",
  kind: "password" as const,
};
const roleOptions = [
  { value: "viewer", label: "只读成员" },
  { value: "oncall", label: "值班人员" },
  { value: "developer", label: "开发者" },
  { value: "knowledge", label: "知识库维护者" },
  { value: "admin", label: "管理员" },
];
export const platformNames: Record<string, string> = {
  feishu: "飞书",
  lark: "Lark（国际版）",
  github: "GitHub",
};
const larkOnly = [{ key: "provider", values: ["feishu", "lark"] }];
const githubOnly = [{ key: "provider", values: ["github"] }];
const manualRepository = [
  ...githubOnly,
  { key: "repositoryId", values: [""] },
];

const policyDevelopmentKinds = ["plan", "edit", "test", "review", "publish_pr"];
const policyTestedKinds = ["review", "publish_pr"];
const policyBudgetDefaults = { maxFiles: 1, maxTokens: 20000, maxSeconds: 600 };

function policyKind(values: SettingValues) {
  return String(values.kind ?? settingsSections.policies.fields.find((field) => field.key === "kind")?.default ?? "reply_send");
}

function policyMode(values: SettingValues) {
  return String(values.mode ?? settingsSections.policies.fields.find((field) => field.key === "mode")?.default ?? "manual");
}

function policyFieldVisible(key: string, values: SettingValues) {
  const kind = policyKind(values);
  const mode = policyMode(values);
  const development = policyDevelopmentKinds.includes(kind);
  if (["name", "kind", "mode", "expiresAt", "enabled", "notificationEnabled"].includes(key)) return true;
  if (key === "notificationChannelId") return values.notificationEnabled !== false;
  if (["channelId", "repositoryId"].includes(key)) return kind === "create_task";
  if (["sponsorPrincipalId", "condition", "followupMode", "senderTypes"].includes(key)) return kind === "create_task" && mode !== "deny";
  if (key === "channelIds") return kind === "reply_send";
  if (key === "repositoryIds") return development;
  if (key === "allowedPaths") return (development && mode === "auto") || (kind === "create_task" && mode !== "deny");
  if (key === "requireTests") return false;
  if (["maxFiles", "maxTokens", "maxSeconds"].includes(key)) return mode === "auto" && !["knowledge_publish", "create_task"].includes(kind);
  return true;
}

function normalizePolicyPayload(body: SettingValues) {
  const kind = String(body.kind ?? "reply_send");
  const mode = String(body.mode ?? "manual");
  const development = policyDevelopmentKinds.includes(kind);
  if (kind !== "reply_send") body.channelIds = [];
  if (!development) {
    body.repositoryIds = [];
    if (kind !== "create_task") body.allowedPaths = [];
  }
  if (!policyTestedKinds.includes(kind)) body.requireTests = false;
  if (kind === "create_task" && mode !== "deny") {
    if (!Array.isArray(body.allowedPaths) || !body.allowedPaths.length) throw new Error("请选择允许修改的路径");
    if (!Array.isArray(body.senderTypes) || !body.senderTypes.length) throw new Error("请选择任务来源");
    body.allowBot = body.senderTypes.includes("bot");
  }
  if (mode === "auto" && kind !== "create_task") {
    for (const [key, value] of Object.entries(policyBudgetDefaults)) {
      if (body[key] === undefined) body[key] = value;
      if (Number(body[key]) < 1) throw new Error("自动批准的文件数、用量和时长上限必须大于 0");
    }
  }
  return body;
}
export const settingsSections: Record<string, SettingSection> = {
  connections: {
    title: "平台账号",
    resource: "connectors",
    action: "verify",
    columns: [
      { key: "name", label: "名称" },
      { key: "provider", label: "平台" },
      { key: "status", label: "账号状态" },
      { key: "wsStatus", label: "长连接" },
      { key: "secretConfigured", label: "凭据" },
    ],
    fields: [
      {
        key: "provider",
        label: "平台",
        kind: "select",
        required: true,
        options: Object.entries(platformNames).map(([value, label]) => ({ value, label })),
        default: "feishu",
      },
      { key: "name", label: "账号名称", placeholder: "支持机器人", required: true },
      {
        key: "baseUrl",
        label: "平台 API 地址",
        placeholder: "留空使用平台默认地址",
        advanced: true,
      },
      { key: "appId", label: "App ID", visibleWhen: larkOnly, group: "应用信息" },
      { key: "tenantId", label: "Tenant ID", visibleWhen: larkOnly, group: "应用信息" },
      {
        key: "repositoryId", label: "使用已有代码仓库", lookup: "repositories",
        visibleWhen: githubOnly, group: "接收反馈的仓库",
        hint: "Token 留空时使用仓库凭据。",
      },
      {
        key: "owner",
        label: "仓库所有者",
        placeholder: "GitHub 用户名或组织名",
        visibleWhen: manualRepository, group: "接收反馈的仓库",
      },
      { key: "repo", label: "仓库名", visibleWhen: manualRepository, group: "接收反馈的仓库" },
      {
        key: "senderLogin",
        label: "评论发送账号（可选）",
        placeholder: "agentowl[bot]",
        visibleWhen: githubOnly, advanced: true,
      },
      {
        key: "appSecret",
        label: "App Secret",
        kind: "password",
        visibleWhen: larkOnly, group: "应用信息",
      },
      { key: "wsEnabled", label: "启用长连接", kind: "boolean", default: false, visibleWhen: larkOnly, group: "事件接收" },
      {
        key: "verificationToken",
        label: "Verification Token",
        kind: "password",
        visibleWhen: larkOnly, group: "事件接收",
      },
      { key: "encryptKey", label: "Encrypt Key", kind: "password", visibleWhen: larkOnly, group: "事件接收", hint: "HTTP 推送或卡片回调必填。" },
      { key: "userLoginEnabled", label: "飞书登录与绑定", kind: "boolean", default: false, visibleWhen: larkOnly, group: "用户身份", hint: "在飞书应用中登记本站的 /auth/lark/callback 回调地址。" },
      { ...token, visibleWhen: githubOnly, group: "访问凭据" },
      { key: "webhookSecret", label: "Webhook 签名密钥", kind: "password", visibleWhen: githubOnly, group: "事件接收", placeholder: "GitHub Webhook Secret" },
      {
        key: "status",
        label: "状态",
        kind: "select",
        default: "unverified",
        advanced: true,
        options: [
          { value: "unverified", label: "待检查" },
          { value: "disabled", label: "停用" },
        ],
      },
    ],
  },
  channels: {
    title: "消息渠道",
    resource: "channels",
    action: "sync",
    columns: [
      { key: "name", label: "渠道" },
      { key: "externalId", label: "接收范围" },
      { key: "replyMode", label: "回复方式" },
      { key: "enabled", label: "状态" },
      { key: "pollIntervalMinutes", label: "定时同步" },
      { key: "lastPolledAt", label: "最近同步" },
      { key: "pollLastError", label: "同步错误" },
    ],
    fields: [
      { key: "name", label: "渠道名称", required: true },
      {
        key: "connectorId",
        label: "平台账号",
        lookup: "connectors",
        required: true,
      },
      {
        key: "externalId",
        label: "平台渠道 ID",
        required: true,
      },
      {
        key: "kind",
        label: "渠道类型",
        kind: "select",
        options: [
          { value: "chat", label: "群聊" },
          { value: "issue", label: "GitHub Issue" },
        ],
        default: "chat",
      },
      {
        key: "replyMode",
        label: "回复方式",
        kind: "select",
        options: [
          { value: "draft", label: "仅生成草稿" },
          { value: "mentions", label: "仅 @ 或回复机器人" },
          { value: "auto", label: "按审批策略回复" },
          { value: "manual", label: "人工处理" },
        ],
        default: "draft",
      },
      {
        key: "pollIntervalMinutes", label: "定时同步间隔（分钟）", kind: "number", default: 0,
        hint: "0 为关闭；仅同步 Issue 和评论。",
      },
      visibility,
      enabled,
    ],
  },
  repositories: {
    title: "代码仓库",
    resource: "repositories",
    columns: [
      { key: "name", label: "仓库" },
      { key: "owner", label: "所有者" },
      { key: "repo", label: "仓库名" },
      { key: "enabled", label: "状态" },
    ],
    fields: [
      { key: "name", label: "显示名称", required: true },
      { key: "owner", label: "仓库所有者", required: true },
      { key: "repo", label: "仓库名", required: true },
      {
        key: "baseBranch",
        label: "起始分支",
        default: "main",
        required: true,
      },
      {
        key: "prPollIntervalMinutes", label: "PR 状态同步间隔（分钟）", kind: "number", default: 5,
        hint: "0 为关闭。",
      },
      {
        key: "allowedPaths",
        label: "允许修改的路径",
        kind: "lines",
        hint: "每行一个相对路径，末尾不加 /。",
        placeholder: "internal/auth\ninternal/token",
      },
      {
        key: "testArgv",
        label: "测试命令参数",
        kind: "lines",
        hint: "每行一个命令或参数，不支持 Shell 语法。",
        placeholder: "go\ntest\n./internal/auth/...",
      },
      {
        key: "runnerImage",
        label: "任务运行镜像",
        placeholder: "镜像 digest 或已批准的镜像",
      },
      {
        key: "config",
        label: "运行配置（JSON）",
        kind: "json",
        hint: "请勿填写密钥。",
        placeholder:
          '{"test_env":{"GOCACHE":"/tmp/go-cache","GOMODCACHE":"/tmp/go-mod"}}',
      },
      token,
      enabled,
    ],
  },
  runtimeMachines: {
    title: "运行机器",
    resource: "runtimeMachines",
    columns: [
      { key: "name", label: "机器" },
      { key: "id", label: "机器 ID" },
      { key: "status", label: "状态" },
      { key: "baseUrl", label: "执行地址" },
      { key: "executionVersion", label: "执行配置" },
      { key: "modelVersion", label: "模型配置" },
      { key: "secretConfigured", label: "令牌" },
    ],
    fields: [
      { key: "name", label: "机器名称", required: true },
      { key: "baseUrl", label: "执行地址", required: true, placeholder: "https://runner-a.example.internal" },
      { key: "callbackBaseUrl", label: "回调地址", required: true, placeholder: "https://api.example.internal" },
      { key: "status", label: "状态", kind: "select", default: "active", options: [
        { value: "active", label: "接收新任务" },
        { value: "degraded", label: "可用（需检查）" },
        { value: "draining", label: "仅完成现有任务" },
        { value: "disabled", label: "停用" },
      ] },
      { key: "schedulingWeight", label: "调度权重", kind: "number", default: 1, hint: "1–1000，数值越大越优先。" },
      { key: "allowedImages", label: "允许运行的镜像", kind: "lines", hint: "每行一个镜像 digest，留空不额外限制。" },
      { key: "labels", label: "机器标签（JSON）", kind: "json", placeholder: '{"region":"cn-bj","env":"prod"}' },
      { key: "token", label: "执行访问令牌", kind: "password" },
      { key: "callbackToken", label: "回调访问令牌", kind: "password" },
    ],
  },
  policies: {
    title: "审批策略",
    resource: "policies",
    columns: [
      { key: "name", label: "策略" },
      { key: "kind", label: "操作" },
      { key: "mode", label: "审批方式" },
      { key: "scope", label: "适用范围" },
      { key: "notificationEnabled", label: "审批通知" },
      { key: "expiresAt", label: "有效期至" },
      { key: "enabled", label: "状态" },
    ],
    fields: [
      { key: "name", label: "策略名称", required: true },
      {
        key: "kind",
        label: "操作类型",
        kind: "select",
        required: true,
        options: [
          { value: "create_task", label: "创建开发任务" },
          { value: "reply_send", label: "发送回复" },
          { value: "knowledge_publish", label: "发布知识" },
          { value: "plan", label: "生成方案" },
          { value: "edit", label: "修改代码" },
          { value: "test", label: "运行测试" },
          { value: "review", label: "审查变更" },
          { value: "publish_pr", label: "提交 PR" },
        ],
        default: "reply_send",
      },
      {
        key: "mode",
        label: "审批方式",
        kind: "select",
        required: true,
        options: [
          { value: "manual", label: "人工审批" },
          { value: "auto", label: "自动批准" },
          { value: "deny", label: "禁止执行" },
        ],
        default: "manual",
      },
      { key: "channelId", label: "消息渠道", lookup: "channels", required: true },
      { key: "repositoryId", label: "开发仓库", lookup: "repositories", required: true },
      { key: "sponsorPrincipalId", label: "授权负责人", lookup: "principals", required: true, hint: "需有该渠道和仓库的开发权限。" },
      { key: "condition", label: "创建条件", kind: "textarea", required: true, placeholder: "例如：已确认的登录故障，且有明确复现步骤" },
      { key: "followupMode", label: "后续补充", kind: "select", default: "manual", options: [{value:"manual",label:"由开发者确认"},{value:"within_scope",label:"自动应用范围内补充"}] },
      { key: "senderTypes", label: "任务来源", kind: "lines", options: [{value:"human",label:"用户消息"},{value:"bot",label:"其他机器人的消息"}] },
      {
        key: "channelIds",
        label: "渠道范围",
        kind: "lines",
        lookup: "channels",
        hint: "不选则适用全部渠道；自动批准需有成功发送记录。",
      },
      {
        key: "repositoryIds",
        label: "仓库范围",
        kind: "lines",
        lookup: "repositories",
        hint: "不选则适用全部仓库。",
      },
      {
        key: "allowedPaths",
        label: "允许修改的路径",
        kind: "lines",
        hint: "每行一个相对路径；留空不自动批准代码变更。",
      },
      {
        key: "maxFiles",
        label: "最多修改文件数",
        kind: "number",
        default: policyBudgetDefaults.maxFiles,
      },
      {
        key: "maxTokens",
        label: "模型用量上限（Token）",
        kind: "number",
        default: policyBudgetDefaults.maxTokens,
      },
      {
        key: "maxSeconds",
        label: "执行时长上限（秒）",
        kind: "number",
        default: policyBudgetDefaults.maxSeconds,
      },
      {
        key: "requireTests",
        label: "要求当前代码测试通过",
        kind: "boolean",
        default: false,
      },
      { key: "notificationEnabled", label: "发送审批通知", kind: "boolean", default: true },
      { key: "notificationChannelId", label: "通知渠道", lookup: "channels" },
      {
        key: "expiresAt",
        label: "策略到期时间",
        hint: "留空表示不到期。",
      },
      enabled,
    ],
  },
  team: {
    title: "团队成员",
    resource: "principals",
    columns: [
      { key: "name", label: "成员" },
      { key: "email", label: "邮箱" },
      { key: "role", label: "主角色" },
      { key: "active", label: "状态" },
    ],
    fields: [
      { key: "name", label: "姓名", required: true },
      { key: "email", label: "邮箱", required: true },
      {
        key: "password",
        label: "账号密码",
        kind: "password",
        placeholder: "新建本地账号时填写，12–72 字节",
      },
      {
        key: "role",
        label: "主角色",
        kind: "select",
        options: roleOptions,
        default: "viewer",
      },
      {
        key: "roles",
        label: "附加角色",
        kind: "lines",
        options: roleOptions,
      },
      {
        key: "channelIds",
        label: "授权渠道",
        kind: "lines",
        lookup: "channels",
      },
      {
        key: "repositoryIds",
        label: "授权仓库",
        kind: "lines",
        lookup: "repositories",
      },
      {
        key: "subject",
        label: "登录身份标识（Subject）",
        placeholder: "登录平台提供的 Subject",
      },
      { key: "active", label: "账号启用", kind: "boolean", default: true },
    ],
  },
  identities: {
    title: "身份绑定",
    resource: "identityBindings",
    columns: [
      { key: "principalId", label: "团队成员" },
      { key: "provider", label: "平台" },
      { key: "displayName", label: "飞书账号" },
      { key: "subjectId", label: "平台身份" },
    ],
    fields: [
      {
        key: "principalId",
        label: "团队成员",
        lookup: "principals",
        required: true,
      },
      {
        key: "provider",
        label: "平台",
        kind: "select",
        options: options(["feishu", "lark"]),
        default: "feishu",
        required: true,
      },
      { key: "appId", label: "应用 ID", visibleWhen: larkOnly, required: true },
      { key: "subjectId", label: "平台账号标识（Subject / Open ID）", visibleWhen: larkOnly, required: true },
      enabled,
    ],
  },
  routing: {
    title: "值班安排",
    resource: "ownerRoutes",
    columns: [
      { key: "module", label: "模块" },
      { key: "primaryId", label: "主负责人" },
      { key: "backupId", label: "备用负责人" },
      { key: "enabled", label: "状态" },
    ],
    fields: [
      {
        key: "module",
        label: "模块",
        required: true,
        placeholder: "auth / token / *",
      },
      {
        key: "primaryId",
        label: "主负责人",
        lookup: "principals",
        required: true,
      },
      { key: "backupId", label: "备用负责人", lookup: "principals" },
      { key: "channelId", label: "通知渠道", lookup: "channels" },
      {
        key: "claimMinutes",
        label: "认领时限（分钟）",
        kind: "number",
        default: 30,
      },
      {
        key: "resolveMinutes",
        label: "处理时限（分钟）",
        kind: "number",
        default: 240,
      },
      enabled,
    ],
  },
  sources: {
    title: "知识来源",
    resource: "knowledgeSources",
    action: "sync",
    columns: [
      { key: "name", label: "来源" },
      { key: "kind", label: "类型" },
      { key: "status", label: "状态" },
      { key: "lastSyncedAt", label: "最近同步" },
      { key: "lastError", label: "同步错误" },
    ],
    fields: [
      { key: "name", label: "来源名称", required: true },
      {
        key: "kind",
        label: "来源类型",
        kind: "select",
        required: true,
        options: [
          { value: "repository", label: "仓库 Markdown" },
          { value: "lark", label: "飞书 / Lark 文档" },
          { value: "manual", label: "人工录入" },
        ],
        default: "repository",
      },
      { key: "repositoryId", label: "所属仓库", lookup: "repositories", visibleWhen: [{ key: "kind", values: ["repository"] }] },
      { key: "connectorId", label: "飞书 / Lark 账号", lookup: "connectors", visibleWhen: [{ key: "kind", values: ["lark"] }] },
      {
        key: "documentId",
        label: "文档 ID",
        placeholder: "文档 ID 或填写下方链接",
        visibleWhen: [{ key: "kind", values: ["lark"] }],
      },
      { key: "path", label: "仓库文档路径", placeholder: "docs/auth.md", visibleWhen: [{ key: "kind", values: ["repository"] }] },
      { key: "url", label: "文档链接", placeholder: "https://…", visibleWhen: [{ key: "kind", values: ["lark"] }] },
      {
        key: "status",
        label: "来源状态",
        kind: "select",
        default: "pending",
        options: [
          { value: "pending", label: "等待同步" },
          { value: "revoked", label: "撤销授权" },
        ],
        hint: "撤销后停止引用相关知识。",
      },
      visibility,
      {
        key: "channelIds",
        label: "授权渠道",
        kind: "lines",
        lookup: "channels",
      },
    ],
  },
};

type SettingValues = Record<string, unknown>;
type ConnectorOption = { id?: string | null; provider?: string | null };

export function settingFields(
  section: string,
  values: SettingValues,
  connectors: ConnectorOption[] = [],
): SettingField[] {
  const config = settingsSections[section];
  if (!config) return [];
  const provider = connectors.find((c) => c.id === values.connectorId)?.provider;
  return config.fields
    .filter((field) => field.visibleWhen?.every((condition) => {
      const value = values[condition.key] ?? config.fields.find((f) => f.key === condition.key)?.default ?? "";
      return condition.values.includes(String(value));
    }) ?? true)
    .filter((field) => section !== "channels" || field.key !== "kind")
    .filter((field) => section !== "policies" || policyFieldVisible(field.key, values))
    .filter((field) => section !== "channels" || field.key !== "pollIntervalMinutes" || ["github", "feishu", "lark"].includes(provider ?? ""))
    .map((field) => {
      if (section === "channels" && field.key === "pollIntervalMinutes" && (provider === "feishu" || provider === "lark")) {
        return { ...field, hint: "0 为关闭；首次可能导入历史消息，不自动发送回复。" };
      }
      if (section === "channels" && field.key === "externalId") {
        if (provider === "github") return {
          ...field,
          label: "Issue / PR 或仓库范围",
          placeholder: "issue:123、pull:456 或 owner/repo",
          hint: "PR 评论需另加 issue:N；owner/repo 表示整个仓库。",
        };
        if (provider === "feishu" || provider === "lark") return {
          ...field,
          label: "群 ID（chat_id）",
          placeholder: "oc_…",
        };
        return { ...field, label: "接收范围" };
      }
      if (section === "channels" && field.key === "replyMode" && provider === "github") {
        return { ...field, options: field.options?.filter((option) => option.value !== "mentions") };
      }
      return field;
    });
}

export function changeSettingValue(
  section: string,
  values: SettingValues,
  key: string,
  value: unknown,
): SettingValues {
  const next = { ...values, [key]: value };
  if (value === values[key]) return next;
  if (section === "connections" && key === "provider") {
    // Draft credentials belong to the selected platform, including its API URL.
    for (const field of settingsSections.connections.fields) {
      if (!["name", "provider", "status"].includes(field.key)) delete next[field.key];
    }
  }
  if (section === "identities" && key === "provider") {
    delete next.appId;
    delete next.subjectId;
  }
  if (section === "sources" && key === "kind") {
    const active = new Set(settingFields(section, next).map((field) => field.key));
    for (const field of settingsSections.sources.fields) {
      if (!active.has(field.key)) delete next[field.key];
    }
  }
  if (section === "channels" && key === "connectorId") {
    next.externalId = "";
    if (next.replyMode === "mentions") next.replyMode = "draft";
  }
  if (section === "policies" && key === "repositoryId") next.allowedPaths = [];
  if (section === "policies" && ["repositoryId", "channelId"].includes(key)) next.sponsorPrincipalId = "";
  if (section === "policies" && (key === "kind" || key === "mode")) {
    const active = new Set(settingFields(section, next).map((field) => field.key));
    for (const field of settingsSections.policies.fields) {
      if (!active.has(field.key) && !["name", "kind", "mode", "enabled", "expiresAt"].includes(field.key)) delete next[field.key];
    }
  }
  return next;
}

export function settingPayload(
  section: string,
  values: SettingValues,
  connectors: ConnectorOption[] = [],
): SettingValues {
  const body: SettingValues = {};
  for (const field of settingFields(section, values, connectors)) {
    const value = values[field.key] ?? field.default;
    if (field.kind === "password" && !value) continue;
    if (field.required && !String(value ?? "").trim()) throw new Error(`请填写${field.label}`);
    if (field.kind === "number") {
      const parsed = z.coerce.number().int().nonnegative().safeParse(value);
      if (!parsed.success) throw new Error(`${field.label}请填写 0 或正整数`);
      body[field.key] = parsed.data;
    } else if (field.kind === "json") {
      try {
        body[field.key] = { additionalData: z.record(z.string(), z.unknown()).parse(JSON.parse(String(value || "{}"))) };
      } catch {
        throw new Error(`${field.label}需填写 JSON 对象（{ }）`);
      }
    } else if (field.kind === "lines") {
      body[field.key] = (Array.isArray(value) ? value : String(value ?? "").split("\n"))
        .map((line) => String(line).trim()).filter(Boolean);
    } else if (field.kind === "boolean") body[field.key] = Boolean(value);
    else body[field.key] = field.kind === "password" ? String(value ?? "") : String(value ?? "").trim();
  }
  if (body.email && !z.email().safeParse(body.email).success) throw new Error("邮箱格式不正确，请检查后重试");
  if (section === "repositories" && Number(body.prPollIntervalMinutes ?? 5) > 1440) throw new Error("PR 同步间隔不能超过 1440 分钟");
  if (section === "runtimeMachines") {
    if (Number(body.schedulingWeight ?? 0) < 1 || Number(body.schedulingWeight ?? 0) > 1000) throw new Error("调度权重必须在 1 到 1000 之间");
    for (const key of ["baseUrl", "callbackBaseUrl"]) {
      try {
        const url = new URL(String(body[key] ?? ""));
        if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error("invalid");
      } catch {
        throw new Error("运行机器地址需使用 HTTP(S)，不能包含账号、密码、查询参数或片段");
      }
    }
  }
  if (section === "channels") {
    const provider = connectors.find((connector) => connector.id === body.connectorId)?.provider;
    if (!provider || !platformNames[provider]) throw new Error("请选择有效的平台账号");
    body.kind = provider === "github" ? "issue" : "chat";
    if (provider === "github" && body.replyMode === "mentions") throw new Error("GitHub 渠道请使用草稿、按审批规则回复或人工处理");
    if (Number(body.pollIntervalMinutes ?? 0) > 1440) throw new Error("同步间隔不能超过 1440 分钟");
    if (provider === "github" && Number(body.pollIntervalMinutes ?? 0) > 0 && String(body.externalId).startsWith("pull:")) throw new Error("PR 渠道暂不支持定时同步");
  }
  if (section === "policies") {
    if (body.expiresAt) { const date = new Date(String(body.expiresAt)); if (!Number.isFinite(date.getTime())) throw new Error("请选择有效的到期时间"); body.expiresAt = date.toISOString(); }
    return normalizePolicyPayload(body);
  }
  return body;
}
