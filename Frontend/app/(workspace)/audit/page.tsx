"use client";
import { Suspense, useState } from "react";
import { useSearchParams } from "next/navigation";
import useSWR from "swr";
import { Activity, ArrowRight, RefreshCw, Search } from "lucide-react";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Empty,
  Status,
  Time,
  label,
  reportError,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Input, Select } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import { activityAction, activityResource, auditRecordCategory, externalOperationCategory } from "@/lib/activity-labels";
import { cn, shortId } from "@/lib/utils";
import type { Audit, ExternalOperation } from "@/lib/api/generated/models";
import { ActivityMarker } from "@/components/activity-marker";
export default function AuditPage() {
  return (
    <Suspense fallback={<Loading />}>
      <AuditContent />
    </Suspense>
  );
}
function AuditContent() {
  const { data: me } = useMe();
  const params = useSearchParams();
  const [tab, setTab] = useState(
    params.get("tab") === "operations" ? "operations" : "audit",
  );
  const [search, setSearch] = useState("");
  const [cursor, setCursor] = useState("");
  const [selected, setSelected] = useState<Audit | ExternalOperation | null>(
    null,
  );
  const [busy, setBusy] = useState(false);
  const { data, error, isLoading, mutate } = useSWR(
    ["audit", tab, search, cursor],
    () =>
      tab === "audit"
        ? api.audit({ q: search, cursor, limit: 30 })
        : api.externalOperations({ status: search, cursor, limit: 30 }),
    { refreshInterval: 15000 },
  );
  return (
    <>
      <PageHeader
        title="操作记录"
      />
      <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div className="inline-flex rounded-lg border border-border bg-white p-1">
          {[
            { id: "audit", name: "操作记录" },
            { id: "operations", name: "平台操作" },
          ].map((t) => (
            <button
              key={t.id}
              onClick={() => {
                setTab(t.id);
                setSearch("");
                setCursor("");
              }}
              className={cn(
                "rounded-md px-4 py-2 text-sm",
                tab === t.id
                  ? "bg-muted font-medium text-foreground"
                  : "text-muted-foreground",
              )}
            >
              {t.name}
            </button>
          ))}
        </div>
        {tab === "audit" ? (
          <div className="relative w-64">
            <Search className="absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={search}
              onChange={(e) => {
                setSearch(e.target.value);
                setCursor("");
              }}
              placeholder="搜索操作或记录类型"
              aria-label="搜索操作记录"
              className="pl-9"
            />
          </div>
        ) : (
          <Select
            className="w-40"
            aria-label="操作状态"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setCursor("");
            }}
          >
            <option value="">全部状态</option>
            <option value="outcome_unknown">结果待核对</option>
            <option value="succeeded">已成功</option>
            <option value="failed">已失败</option>
            <option value="pending">等待发送</option>
          </Select>
        )}
      </div>
      <Panel>
        {isLoading ? (
          <Loading />
        ) : error ? (
          <ErrorState error={error} retry={() => void mutate()} />
        ) : data?.items?.length ? (
          <div className="divide-y divide-border">
            {data.items.map((raw) => {
              const a = raw as Audit;
              const o = raw as ExternalOperation;
              const category = tab === "audit" ? auditRecordCategory(a.action, a.resourceType, a.actorId) : externalOperationCategory();
              return (
                <button
                  key={raw.id}
                  onClick={() => setSelected(raw)}
                  className="flex w-full items-start gap-3 px-5 py-4 text-left transition hover:bg-muted/40"
                >
                  <span className="mt-.5 rounded-md bg-muted p-2">
                    <Activity className="size-3.5 text-muted-foreground" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <ActivityMarker label={category.label} tone={category.tone} />
                      <span className="text-sm font-medium">
                        {tab === "audit" ? activityAction(a.action, a.resourceType) : label(o.kind)}
                      </span>
                      {tab === "operations" && <Status value={o.status} />}
                    </div>
                    <p className="mt-1.5 line-clamp-1 text-xs text-muted-foreground">
                      {tab === "audit"
                        ? a.detail ||
                          `${activityResource(a.resourceType)}${a.resourceId && !["execution", "model"].includes(a.resourceId) ? ` · ${shortId(a.resourceId)}` : ""}`
                        : o.errorEscaped ||
                          `关联记录 ${shortId(o.subjectId)} · 平台记录 ${o.remoteId || "暂无"}`}
                    </p>
                  </div>
                  <div className="flex flex-col items-end gap-2">
                    <Time value={raw.createdAt} />
                    <ArrowRight className="size-3 text-muted-foreground" />
                  </div>
                </button>
              );
            })}
          </div>
        ) : (
          <Empty
            title="暂无记录"
          />
        )}
        <div className="flex items-center justify-between border-t border-border px-5 py-3">
          <span className="text-xs text-muted-foreground">
            共 {data?.total ?? "—"} 条记录
          </span>
          <div className="flex gap-2">
            {cursor && (
              <Button variant="ghost" size="sm" onClick={() => setCursor("")}>
                返回第一页
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              disabled={!data?.nextCursor}
              onClick={() => setCursor(data?.nextCursor ?? "")}
            >
              下一页
            </Button>
          </div>
        </div>
      </Panel>
      <Dialog
        open={selected !== null}
        onOpenChange={(o) => {
          if (!o) setSelected(null);
        }}
        title={tab === "audit" ? "操作详情" : "平台操作详情"}
      >
        <pre className="max-h-96 overflow-auto rounded-lg bg-muted p-4 font-mono text-xs leading-6 break-words whitespace-pre-wrap">
          {JSON.stringify(selected, null, 2)}
        </pre>
        {tab === "operations" &&
          hasRole(me, "admin") &&
          (selected as ExternalOperation)?.status === "outcome_unknown" && (
            <Button
              className="mt-5 w-full"
              disabled={busy}
              onClick={async () => {
                setBusy(true);
                try {
                  await api.reconcileOperation(selected?.id ?? "", {
                    expectedVersion: selected?.version ?? 0,
                  });
                  toast.success("已提交核对请求");
                  setSelected(null);
                  await mutate();
                } catch (e) {
                  reportError(e);
                } finally {
                  setBusy(false);
                }
              }}
            >
              <RefreshCw className={cn("size-4", busy && "animate-spin")} />
              核对平台结果
            </Button>
          )}
      </Dialog>
    </>
  );
}
