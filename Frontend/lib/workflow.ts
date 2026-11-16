import type { Approval } from "./api/generated/models/index";

export function approvalTarget(approval: Approval): string | undefined {
  const action = approval.action?.additionalData;
  if (approval.subjectType === "knowledge" && approval.subjectId)
    return `/knowledge/${encodeURIComponent(approval.subjectId)}`;
  if (["reply", "admission"].includes(approval.subjectType ?? "") && typeof action?.case_id === "string")
    return `/cases/${encodeURIComponent(action.case_id)}`;
  if (approval.subjectType === "step" && typeof action?.task_id === "string")
    return `/development/${encodeURIComponent(action.task_id)}`;
  return undefined;
}

export function formatTaskQuota(value: unknown, unit: string): string {
  if (value === 0) return "不限";
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0 ? `${value} ${unit}` : "未提供";
}

export function approvalExecutionDetails(action: Record<string, unknown>): { label: string; value: string }[] {
  const record = (value: unknown): Record<string, unknown> => value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
  const profile = record(action.execution_profile);
  const limits = record(action.limits);
  const timeout = action.timeout_seconds ?? limits.timeout_seconds;
  const network = profile.network_mode;
  const readonly = profile.read_only_rootfs;
  const writableTest = profile.test_workspace_writable;
  const approvalMode = action.approval_mode;
  return [
    { label: "最长运行时间", value: timeout === 0 ? "不限时" : typeof timeout === "number" && timeout > 0 ? `${timeout} 秒` : "未提供" },
    { label: "网络", value: network === "bridge" ? "可联网" : network === "none" ? "离线" : "未提供" },
    { label: "根文件系统", value: readonly === true ? "只读" : readonly === false ? "可写" : "未提供" },
    { label: "测试目录", value: writableTest === true ? "临时副本可写，新增文件不进 PR" : writableTest === false ? "只读" : "未提供" },
    { label: "审批方式", value: approvalMode === "auto" ? "未匹配策略时自动批准" : approvalMode === "manual" ? "每步人工审批" : approvalMode === "policy" ? "按审批策略" : "未提供" },
  ];
}
