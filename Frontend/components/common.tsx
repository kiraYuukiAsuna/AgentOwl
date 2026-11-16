"use client";
import Link from "next/link";
import {
  ArrowLeft,
  ArrowUpRight,
  CircleAlert,
  Inbox,
  Loader2,
  RefreshCw,
} from "lucide-react";
import { cn, dateLabel } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { toast } from "sonner";

const labels: Record<string, string> = {
  new: "待分类",
  triage: "待分类",
  open: "待处理",
  pending: "待处理",
  pending_approval: "待审批",
  waiting_approval: "待审批",
  in_progress: "处理中",
  processing: "处理中",
  awaiting_info: "待补充",
  waiting_user: "待用户反馈",
  waiting_fix: "待修复",
  waiting_release: "待发布",
  awaiting_release: "待发布",
  resolved: "已解决",
  closed: "已关闭",
  draft: "草稿",
  submitted: "待审批",
  published: "已发布",
  withdrawn: "已撤回",
  expired: "已过期",
  approved: "已批准",
  rejected: "已拒绝",
  revoked: "已撤销",
  stale: "已失效",
  ready: "准备就绪",
  queued: "排队中",
  running: "运行中",
  paused: "已暂停",
  blocked: "无法继续",
  failed: "失败",
  ignored: "已忽略",
  cancelled: "已取消",
  succeeded: "成功",
  completed: "已完成",
  pr_submitted: "PR 已提交",
  pr_open: "PR 待合并",
  executed: "已执行",
  sent: "已发送",
  generating: "生成中",
  active: "已启用",
  inactive: "已停用",
  enabled: "已启用",
  disabled: "已停用",
  healthy: "连接正常",
  unverified: "待检查",
  verified: "检查通过",
  observed: "有成功记录",
  synced: "已同步",
  unavailable: "不可用",
  disconnected: "未连接",
  plan: "制定方案",
  reply_send: "发送回复",
  knowledge_publish: "发布知识",
  mentions: "仅 @ 或回复机器人",
  edit: "修改代码",
  test: "运行测试",
  review: "代码审查",
  create_task: "创建开发任务",
  publish_pr: "提交 PR",
  track: "跟踪 PR",
  manual: "人工",
  feishu: "飞书",
  lark: "Lark",
  github: "GitHub",
  auto: "自动",
  deny: "禁止",
  internal: "团队内部",
  public: "公开",
  channel: "指定渠道",
  admin: "管理员",
  developer: "开发者",
  oncall: "值班人员",
  knowledge: "知识库维护者",
  viewer: "只读成员",
  in_scope: "范围内",
  out_of_scope: "范围外",
  uncertain: "待判断",
  faq: "常见问题",
  bug: "疑似 Bug",
  question: "使用咨询",
  feedback: "用户反馈",
  security: "安全事件",
  feature: "功能建议",
  unknown: "待判断",
  intake: "待分类",
  awaiting_user: "待用户反馈",
  in_review: "审核中",
  outcome_unknown: "结果待核对",
  dispatching: "发送中",
};
export function label(value?: string | null) {
  return value ? (labels[value] ?? value) : "—";
}
export function Status({
  value,
  className,
}: {
  value?: string | null;
  className?: string;
}) {
  const good = [
    "resolved",
    "succeeded",
    "published",
    "approved",
    "completed",
    "active",
    "healthy",
    "enabled",
    "verified",
    "synced",
  ].includes(value ?? "");
  const bad = [
    "failed",
    "rejected",
    "revoked",
    "disconnected",
    "security",
  ].includes(value ?? "");
  const warm = [
    "blocked",
    "pending",
    "pending_approval",
    "waiting_approval",
    "in_review",
    "outcome_unknown",
  ].includes(value ?? "");
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 whitespace-nowrap rounded-md border px-2 py-0.5 text-xs font-medium leading-5",
        good
          ? "border-emerald-200/60 bg-emerald-50/70 text-emerald-700"
          : bad
            ? "border-red-200/60 bg-red-50/70 text-red-700"
            : warm
              ? "border-amber-200/60 bg-amber-50/70 text-amber-700"
              : "border-border bg-muted/45 text-muted-foreground",
        className,
      )}
    >
      <span className="size-1.5 rounded-full bg-current opacity-75" />
      {label(value)}
    </span>
  );
}
export function Priority({ value }: { value?: string | number | null }) {
  const text = String(value ?? "P2").toUpperCase();
  return (
    <span
      className={cn(
        "font-mono text-[11px] font-medium",
        text === "P0"
          ? "text-red-600"
          : text === "P1"
            ? "text-amber-600"
            : "text-muted-foreground",
      )}
    >
      {text}
    </span>
  );
}
export function PageHeader({
  eyebrow,
  title,
  description,
  action,
  back,
}: {
  eyebrow?: string;
  title: string;
  description?: string;
  action?: React.ReactNode;
  back?: string;
}) {
  return (
    <header className="mb-6 border-b border-border/80 pb-6">
      {(back || eyebrow) && <div className="mb-3 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        {back && (
          <Link
            href={back}
            className="mr-1 inline-flex items-center gap-1 hover:text-primary"
          >
            <ArrowLeft className="size-3.5" />
            返回
          </Link>
        )}
        {eyebrow && (
          <span>{eyebrow}</span>
        )}
      </div>}
      <div className="flex flex-col items-start justify-between gap-4 lg:flex-row">
        <div className="w-full min-w-0 flex-1">
          <h1 className="text-3xl font-semibold break-words">
            {title}
          </h1>
          {description && (
            <p className="mt-2 max-w-3xl text-sm leading-6 text-muted-foreground">
              {description}
            </p>
          )}
        </div>
        {action && <div className="flex max-w-full flex-wrap items-center gap-2 lg:shrink-0">{action}</div>}
      </div>
    </header>
  );
}
export function Panel({
  title,
  description,
  action,
  children,
  className,
}: {
  title?: string;
  description?: string;
  action?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section
      className={cn(
        "min-w-0 overflow-hidden rounded-xl border border-border bg-card",
        className,
      )}
    >
      {title && (
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold">{title}</h2>
            {description && (
              <p className="mt-1 text-sm leading-6 text-muted-foreground">
                {description}
              </p>
            )}
          </div>
          {action}
        </div>
      )}
      {children}
    </section>
  );
}
export function Empty({
  title = "暂无记录",
  description = "",
  action,
}: {
  title?: string;
  description?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex min-h-44 flex-col items-center justify-center px-6 py-10 text-center">
      <span className="mb-3 text-muted-foreground/60">
        <Inbox className="size-6" aria-hidden />
      </span>
      <h3 className="text-sm font-medium">{title}</h3>
      {description && <p className="mt-1.5 max-w-sm text-xs leading-6 text-muted-foreground">
        {description}
      </p>}
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}
export function Loading({ text = "正在加载…" }: { text?: string }) {
  return (
    <div className="flex min-h-44 items-center justify-center gap-2 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {text}
    </div>
  );
}
export function ErrorState({
  error,
  retry,
}: {
  error: unknown;
  retry?: () => void;
}) {
  return (
    <div
      role="alert"
      className="m-5 flex items-start gap-3 rounded-xl border border-red-100 bg-red-50/50 p-5"
    >
      <CircleAlert className="mt-.5 size-4 shrink-0 text-red-600" />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-red-800">加载失败</p>
        <p className="mt-1 break-words text-xs leading-relaxed text-red-700">
          {errorMessage(error)}
        </p>
        {retry && (
          <Button variant="outline" size="sm" className="mt-3" onClick={retry}>
            <RefreshCw className="size-3" />
            重试
          </Button>
        )}
      </div>
    </div>
  );
}
export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (error && typeof error === "object") {
    const e = error as { detail?: string; title?: string; message?: string };
    return e.detail ?? e.message ?? e.title ?? "操作失败，请重试。";
  }
  return "操作失败，请重试。";
}
export function reportError(error: unknown) {
  toast.error(errorMessage(error));
}
export function Definition({
  label: title,
  value,
}: {
  label: string;
  value?: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 py-2.5 text-sm">
      <span className="shrink-0 text-muted-foreground">{title}</span>
      <div className="min-w-0 text-right break-words font-medium">
        {value || "—"}
      </div>
    </div>
  );
}
export function SafeExternalLink({
  href,
  children,
}: {
  href?: string | null;
  children: React.ReactNode;
}) {
  if (!href || !/^https?:\/\//i.test(href)) return null;
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="inline-flex items-center gap-1 text-primary hover:underline"
    >
      {children}
      <ArrowUpRight className="size-3" />
    </a>
  );
}
export function Time({ value }: { value?: string | Date | null }) {
  return (
    <time
      dateTime={value ? new Date(value).toISOString() : undefined}
      className="whitespace-nowrap text-xs text-muted-foreground"
    >
      {dateLabel(value)}
    </time>
  );
}
