"use client";
import Link from "next/link";
import useSWR from "swr";
import {
  ArrowRight,
  BookOpen,
  CircleAlert,
  Code2,
  Inbox,
  Plus,
  ShieldCheck,
} from "lucide-react";
import { api, useMe } from "@/lib/api/client";
import { hasRole } from "@/lib/access";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Empty,
  ErrorState,
  Loading,
  PageHeader,
  Panel,
  Status,
  Time,
  label,
} from "@/components/common";
import { activityAction, caseEventCategory } from "@/lib/activity-labels";
import { CaseTable } from "@/components/case-table";
import { ActivityMarker, activityDotClass } from "@/components/activity-marker";
export default function DashboardPage() {
  const { data: me } = useMe();
  const { data, error, isLoading, mutate } = useSWR(
    "dashboard",
    api.dashboard,
    { refreshInterval: 15000 },
  );
  if (isLoading) return <Loading />;
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!data) return null;
  const stats = [
    {
      label: "待处理问题",
      value: data.openCases,
      caption: `${data.unassignedCases ?? 0} 个待认领`,
      icon: Inbox,
      href: "/cases",
    },
    {
      label: "待审批",
      value: data.pendingApprovals,
      icon: ShieldCheck,
      href: "/approvals",
    },
    {
      label: "进行中的开发任务",
      value: data.runningTasks,
      icon: Code2,
      href: "/development",
      color: "text-muted-foreground bg-muted/60",
    },
    {
      label: "已发布知识",
      value: data.publishedKnowledge,
      icon: BookOpen,
      href: "/knowledge",
    },
  ];
  return (
    <>
      <PageHeader
        title="概览"
        action={
          hasRole(me, "developer", "oncall") && (
            <Button asChild>
              <Link href="/cases?new=1">
                <Plus className="size-4" />
                新建问题
              </Link>
            </Button>
          )
        }
      />
      <div className="mb-6 grid gap-px overflow-hidden rounded-xl border border-border bg-border sm:grid-cols-2 xl:grid-cols-4">
        {stats.map(({ label: title, value, caption, icon: Icon, href }) => (
          <Link key={title} href={href} className="group bg-card p-5 transition-colors hover:bg-background">
            <div className="flex items-center justify-between gap-3 text-muted-foreground">
              <span className="text-sm font-medium">{title}</span>
              <Icon className="size-4 group-hover:text-primary" aria-hidden />
            </div>
            <p className="mt-3 text-3xl font-semibold tracking-tight tabular-nums">{value ?? 0}</p>
            {caption && <p className="mt-2 text-xs text-muted-foreground">{caption}</p>}
          </Link>
        ))}
      </div>
      {((data.failedJobs ?? 0) > 0 || (data.unknownOperations ?? 0) > 0) && (
        <div className="mb-6 flex items-center gap-3 rounded-xl border border-amber-200 bg-amber-50 p-4 text-xs text-amber-800">
          <CircleAlert className="size-4" />
          <div className="flex flex-wrap gap-x-6 gap-y-2">
            {(data.failedJobs ?? 0) > 0 &&
              (hasRole(me, "admin") ? (
                <Link
                  className="inline-flex items-center gap-2 hover:underline"
                  href="/jobs"
                >
                  {data.failedJobs} 项后台任务失败 <ArrowRight className="size-3" />
                </Link>
              ) : (
                <span>{data.failedJobs} 项后台任务失败，请联系管理员处理。</span>
              ))}
            {(data.unknownOperations ?? 0) > 0 && (
              <Link
                className="inline-flex items-center gap-2 hover:underline"
                href="/audit?tab=operations"
              >
                {data.unknownOperations} 项平台操作待核对{" "}
                <ArrowRight className="size-3" />
              </Link>
            )}
          </div>
        </div>
      )}
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_310px]">
        <div className="space-y-6">
          <Panel
            title="最近问题"
            action={
              <Button asChild variant="ghost" size="sm">
                <Link href="/cases">
                  查看全部
                  <ArrowRight className="size-3.5" />
                </Link>
              </Button>
            }
          >
            <CaseTable items={data.recentCases ?? []} />
          </Panel>
          <Panel
            title="开发进展"
            action={
              <Link href="/development" className="text-xs text-primary">
                全部任务 →
              </Link>
            }
          >
            {data.recentTasks?.length ? (
              <div className="divide-y divide-border">
                {data.recentTasks.map((t) => (
                  <Link
                    href={`/development/${t.id}`}
                    key={t.id}
                    className="flex items-center gap-3 p-5 transition hover:bg-muted/40"
                  >
                    <span className="rounded-lg bg-muted p-2">
                      <Code2 className="size-4 text-muted-foreground" />
                    </span>
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium">
                        {t.title || t.goal}
                      </p>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {label(t.stage)} · 第 {t.revision ?? 1} 版
                      </p>
                    </div>
                    <Status value={t.status} />
                  </Link>
                ))}
              </div>
            ) : (
              <Empty
                title="还没有开发任务"
              />
            )}
          </Panel>
        </div>
        <div className="space-y-6">
          <Panel title="最近动态">
            {data.activity?.length ? (
              <div className="px-5 py-2">
                {data.activity.slice(0, 6).map((e, i) => {
                  const category = caseEventCategory(e.type, e.actorId);
                  return (
                    <div className="relative flex gap-3 py-3" key={e.id ?? i}>
                      <span className={cn("z-1 mt-2 size-1.5 shrink-0 rounded-full", activityDotClass[category.tone])} />
                      <div className="min-w-0">
                        <div className="mb-1 flex flex-wrap items-center gap-2">
                          <ActivityMarker label={category.label} tone={category.tone} />
                          {e.type && <span className="text-[11px] text-muted-foreground">{activityAction(e.type, "cases")}</span>}
                        </div>
                        <p className="text-sm leading-6">{activityAction(e.body || e.type, "cases")}</p>
                        <Time value={e.createdAt} />
                      </div>
                    </div>
                  );
                })}
              </div>
            ) : (
              <Empty
                title="暂无动态"
              />
            )}
          </Panel>
        </div>
      </div>
    </>
  );
}
