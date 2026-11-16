"use client";
import { useState } from "react";
import Link from "next/link";
import useSWR from "swr";
import { ArrowUpRight, ShieldCheck } from "lucide-react";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  Loading,
  ErrorState,
  Empty,
  Status,
  Time,
  label,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/input";
import { settingsSections } from "@/lib/settings";
import { shortId } from "@/lib/utils";
export default function ApprovalsPage() {
  const { data: me } = useMe();
  const [kind, setKind] = useState("");
  const [status, setStatus] = useState("pending");
  const [cursor, setCursor] = useState("");
  const { data, error, isLoading, mutate } = useSWR(
    ["approvals", kind, status, cursor],
    () => api.approvals({ kind, status, cursor, limit: 30 }),
    { refreshInterval: 10000 },
  );
  return (
    <>
      <PageHeader
        title="审批"
        action={hasRole(me, "admin") && <Link href="/settings/policies" className="text-sm font-medium text-primary hover:underline">审批策略</Link>}
      />
      <Panel>
        <div className="flex items-center justify-between border-b border-border p-4">
          <div className="flex items-center gap-2 text-sm font-medium">
            <ShieldCheck className="size-4 text-primary" />
            审批列表
            <span className="ml-1 rounded-md bg-muted px-2 py-.5 text-xs text-muted-foreground">
              {data?.total ?? "—"}
            </span>
          </div>
          <div className="flex flex-wrap gap-2">
          <Select aria-label="操作类型" value={kind} onChange={event => { setKind(event.target.value); setCursor(""); }}>
            <option value="">全部操作</option>
            {settingsSections.policies.fields.find(field => field.key === "kind")?.options?.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
          </Select>
          <Select
            aria-label="审批状态"
            className="w-36"
            value={status}
            onChange={(e) => {
              setStatus(e.target.value);
              setCursor("");
            }}
          >
            <option value="pending">待审批</option>
            <option value="">全部状态</option>
            <option value="approved">已批准</option>
            <option value="executed">已执行</option>
            <option value="blocked">需补充信息</option>
            <option value="stale">已失效</option>
            <option value="rejected">已拒绝</option>
            <option value="revoked">已撤销</option>
            <option value="expired">已过期</option>
          </Select>
          </div>
        </div>
        {isLoading ? (
          <Loading />
        ) : error ? (
          <ErrorState error={error} retry={() => void mutate()} />
        ) : data?.items?.length ? (
          <div className="divide-y divide-border">
            {data.items.map((a) => (
              <Link
                key={a.id}
                href={`/approvals/${a.id}`}
                className="group flex items-start gap-4 p-5 transition hover:bg-muted/40"
              >
                <span className="mt-.5 rounded-lg bg-muted/60 p-2.5">
                  <ShieldCheck className="size-4 text-muted-foreground" />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <h2 className="truncate text-sm font-medium group-hover:text-primary">
                      {a.summary || label(a.kind)}
                    </h2>
                    <ArrowUpRight className="size-3.5 text-muted-foreground opacity-0 group-hover:opacity-100" />
                  </div>
                  <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <span>{label(a.kind)}</span>
                    <span>·</span>
                    <span className="font-mono text-[10px]">
                      {shortId(a.id)}
                    </span>
                    <span>·</span>
                    <Time value={a.createdAt} />
                  </div>
                </div>
                <Status value={a.status} />
              </Link>
            ))}
          </div>
        ) : (
          <Empty
            title="暂无审批"
          />
        )}
        <div className="flex justify-end gap-2 border-t border-border p-3">
          {cursor && (
            <Button variant="ghost" size="sm" onClick={() => setCursor("")}>
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
