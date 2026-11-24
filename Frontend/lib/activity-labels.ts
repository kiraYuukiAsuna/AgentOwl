const resources: Record<string, string> = {
  cases: "问题", jobs: "后台任务", channels: "消息渠道", connectors: "平台账号",
  repositories: "代码仓库", policies: "审批策略", approvals: "审批",
  "development-tasks": "开发任务", development_tasks: "开发任务",
  knowledge: "知识条目", "knowledge-sources": "知识来源", principals: "团队成员",
  "identity-bindings": "身份绑定", "owner-routes": "值班安排",
  "execution-settings": "任务执行设置", system_settings: "模型设置",
  external_operations: "平台操作", "external-operations": "平台操作",
};
export function activityResource(value?: string | null): string {
  return resources[value ?? ""] ?? "关联记录";
}
const caseSources: Record<string, string> = {
  manual: "手动创建", feishu: "飞书", lark: "Lark", github: "GitHub",
};
export function caseSourceLabel(value: string): string {
  return caseSources[value] ?? value;
}

export type ActivityTone = "user" | "feedback" | "system" | "platform" | "operation";
export type ActivityCategory = { label: string; tone: ActivityTone };
export type CaseEventTone = ActivityTone;

const systemEventTypes: Record<string, true> = { classified: true, needs_human: true, answer_generated: true, notification_unrouted: true, notification_queued: true };
const platformEventTypes: Record<string, true> = { reply_sent: true, notification_failed: true, notification_outcome_unknown: true, pr_status_synced: true, pr_completed: true, pr_blocked: true, pr_open: true, pr_failed: true };
const operationEventTypes: Record<string, true> = { updated: true, claim: true, resolve: true, close: true, reopen: true, takeover: true, resume: true, retry_classification: true, development_confirmed: true, related: true };

export function caseEventCategory(type?: string | null, actorId?: string | null): ActivityCategory {
  if (type === "source_updated") return { label: "用户消息", tone: "user" };
  if (type === "feedback") return { label: "用户反馈", tone: "feedback" };
  if ((type && systemEventTypes[type]) || actorId?.startsWith("system")) return { label: "系统状态", tone: "system" };
  if (type && platformEventTypes[type]) return { label: "平台状态", tone: "platform" };
  if (type && operationEventTypes[type]) return { label: "人工操作", tone: "operation" };
  return { label: "操作记录", tone: "operation" };
}
const approvalActions: Record<string, true> = { request_approval: true, approve: true, reject: true, revoke: true };
export function auditRecordCategory(action?: string | null, resourceType?: string | null, actorId?: string | null): ActivityCategory {
  const value = action ?? "";
  if (value.startsWith("feedback_")) return { label: "用户反馈", tone: "feedback" };
  if (value === "auto_approve") return { label: "自动审批", tone: "system" };
  if (resourceType === "external_operations" || resourceType === "external-operations" || value.startsWith("external_")) return { label: "平台操作", tone: "platform" };
  if (resourceType === "approvals" || approvalActions[value]) return { label: "审批", tone: "system" };
  if (actorId?.startsWith("system")) return { label: "自动处理", tone: "system" };
  return { label: "操作", tone: "operation" };
}
export function externalOperationCategory(): ActivityCategory {
  return { label: "平台操作", tone: "platform" };
}
const actions: Record<string, string> = {
  resolve: "标记问题已解决", close: "关闭问题", reopen: "重新打开问题", claim: "认领问题",
  feedback_human: "请求人工协助", feedback_resolved: "反馈问题已解决", feedback_unresolved: "反馈问题未解决", feedback_handoff: "请求人工协助",
  ignore: "忽略失败提醒", unignore: "恢复失败提醒", retry_classification: "重新识别问题",
  schedule_channel: "同步渠道消息", schedule_source: "同步知识来源", schedule_verify_connector: "检查平台账号",
  schedule_connector: "检查平台账号", schedule_reconcile_external: "核对平台操作结果",
  external_succeeded: "平台操作成功", external_failed: "平台操作失败", external_outcome_unknown: "平台操作结果待核对",
  external_cancelled: "平台操作已取消", auto_approve: "自动批准操作", request_approval: "提交审批申请",
  approve: "批准操作", reject: "拒绝操作", revoke: "撤销审批", confirm_development: "确认开始开发",
  revise: "更新开发目标", pause: "暂停开发任务", cancel: "取消开发任务",
  feedback_transferred: "反馈需要人工协助", withdraw: "撤回知识条目",
  notification_failed: "值班通知发送失败", notification_outcome_unknown: "值班通知结果待核对",
  remote_effect_after_fence_change: "任务已变更，忽略迟到的平台结果",
};
export function activityAction(action?: string | null, resource?: string | null): string {
  if (action === "save") return `保存${activityResource(resource)}`;
  if (action === "retry") return "重新执行后台任务";
  if (action === "publish") return "发布知识条目";
  if (action === "takeover") return resource === "cases" ? "接管问题处理" : "接管开发任务";
  if (action === "resume") return resource === "cases" ? "恢复自动回复" : "恢复开发任务";
  return actions[action ?? ""] ?? action ?? "其他操作";
}
export const backgroundJobs: Record<string, string> = {
  classify: "识别问题类型", answer: "生成回复草稿", generate_reply: "生成回复草稿",
  prepare_step: "准备开发阶段", execute_approval: "执行已批准的操作", poll_run: "检查开发运行结果",
  stop_task: "停止开发任务", dispatch_external: "提交平台操作", reconcile_external: "核对平台操作结果",
  sync_source: "同步知识来源", sync_channel: "同步渠道消息", notify: "通知负责人", notify_owner: "通知负责人",
  track_pr: "同步 PR 状态", poll_github: "定时同步 GitHub 反馈", poll_lark: "定时同步飞书消息",
  verify_connector: "检查平台账号", ingest_event: "处理平台事件",
};
