"use client";
import { useState } from "react";
import Link from "next/link";
import useSWR from "swr";
import { ArrowUpRight, Code2, GitPullRequest } from "lucide-react";
import { api } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Empty,
  Status,
  Time,
  label,
} from "@/components/common";
import { Select } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
export default function DevelopmentPage() {
  const [status, setStatus] = useState("");
  const [cursor, setCursor] = useState("");
  const { data, error, isLoading, mutate } = useSWR(
    ["tasks", status, cursor],
    () => api.tasks({ status, cursor, limit: 30 }),
    { refreshInterval: 10000 },
  );
  return (
    <>
      <PageHeader
        title="开发任务"
      />
      <Panel>
        <div className="flex items-center justify-between border-b border-border p-4">
          <span className="text-sm font-medium">
            任务列表{" "}
            <span className="ml-2 text-xs text-muted-foreground">
              {data?.total ?? "—"}
            </span>
          </span>
          <Select
            aria-label="任务状态"
            className="w-36"
            value={status}
            onChange={(e) => {
              setStatus(e.target.value);
              setCursor("");
            }}
          >
            <option value="">全部状态</option>
            <option value="queued">排队中</option>
            <option value="running">运行中</option>
            <option value="pending_approval">待审批</option>
            <option value="blocked">已阻塞</option>
            <option value="paused">已暂停</option>
            <option value="pr_open">PR 待合并</option>
            <option value="failed">失败</option>
            <option value="cancelled">已取消</option>
            <option value="completed">已完成</option>
          </Select>
        </div>
        {isLoading ? (
          <Loading />
        ) : error ? (
          <ErrorState error={error} retry={() => void mutate()} />
        ) : data?.items?.length ? (
          <div className="divide-y divide-border">
            {data.items.map((t) => (
              <Link
                key={t.id}
                href={`/development/${t.id}`}
                className="group flex items-start gap-4 p-5 hover:bg-muted/40"
              >
                <span className="rounded-lg bg-muted/60 p-2.5">
                  <Code2 className="size-4 text-muted-foreground" />
                </span>
                <div className="min-w-0 flex-1">
                  <h2 className="truncate text-sm font-medium group-hover:text-primary">
                    {t.title || t.goal}
                    <ArrowUpRight className="ml-1 inline size-3 opacity-0 group-hover:opacity-100" />
                  </h2>
                  <div className="mt-2 flex items-center gap-3 text-xs text-muted-foreground">
                    <span>{label(t.stage)}</span>
                    <span>版本 {t.revision ?? 1}</span>
                    <Time value={t.updatedAt} />
                    {t.prUrl && (
                      <GitPullRequest className="size-3.5 text-primary" />
                    )}
                  </div>
                </div>
                <Status value={t.status} />
              </Link>
            ))}
          </div>
        ) : (
          <Empty
            title="暂无开发任务"
            action={
              <Button asChild variant="outline" size="sm">
                <Link href="/cases">查看问题</Link>
              </Button>
            }
          />
        )}
        <div className="flex justify-end gap-2 border-t border-border p-3">
          {cursor && (
            <Button size="sm" variant="ghost" onClick={() => setCursor("")}>
              返回第一页
            </Button>
          )}
          <Button
            size="sm"
            variant="outline"
            disabled={!data?.nextCursor}
            onClick={() => setCursor(data?.nextCursor ?? "")}
          >
            下一页
          </Button>
        </div>
      </Panel>
    </>
  );
}
