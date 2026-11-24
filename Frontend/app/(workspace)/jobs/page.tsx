"use client";
import { useState } from "react";
import useSWR, { mutate as mutateCache } from "swr";
import { ArrowRight, Loader2, RefreshCw, CheckCircle2 } from "lucide-react";
import { toast } from "sonner";
import { api, useMe } from "@/lib/api/client";
import { backgroundJobs } from "@/lib/activity-labels";
import type { Job } from "@/lib/api/generated/models";
import { hasRole } from "@/lib/access";
import { objectData, shortId } from "@/lib/utils";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Empty,
  Status,
  Time,
  Definition,
  reportError,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Select, Textarea } from "@/components/ui/input";

const jobName = (job: Job) => backgroundJobs[job.kind ?? ""] ?? "其他后台任务";

export default function JobsPage() {
  const { data: me } = useMe();
  const admin = hasRole(me, "admin");
  const [status, setStatus] = useState("failed");
  const [cursor, setCursor] = useState("");
  const [detailId, setDetailId] = useState("");
  const [retry, setRetry] = useState<Job | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const { data, error, isLoading, mutate } = useSWR(
    admin ? ["jobs", status, cursor] : null,
    () => api.jobs({ status: status === "ignored" ? "failed" : status, ignored: status === "failed" ? false : status === "ignored" ? true : undefined, cursor, limit: 30 }),
    { refreshInterval: 10000 },
  );
  const {
    data: detail,
    error: detailError,
    isLoading: detailLoading,
    mutate: refreshDetail,
  } = useSWR(admin && detailId ? ["job", detailId] : null, () =>
    api.job(detailId),
  );
  async function setIgnored(jobs: Job[], ignored: boolean) {
    const candidates = jobs.filter((job) => job.id && job.status === "failed" && Boolean(job.ignoredAt) !== ignored);
    if (!candidates.length) return;
    setBusy(true);
    try {
      const results = await Promise.allSettled(candidates.map((job) => api.ignoreJob(job.id!, { expectedAttempts: job.attempts ?? 0, ignored })));
      const failures = results.filter((result) => result.status === "rejected").length;
      if (failures) toast.error(`${candidates.length - failures} 项已处理，${failures} 项未完成，请刷新后重试`);
      else toast.success(ignored ? `已忽略 ${candidates.length} 项提醒` : "已恢复失败提醒");
      await Promise.all([mutate(), mutateCache("dashboard"), detailId ? refreshDetail() : Promise.resolve()]);
    } finally { setBusy(false); }
  }
  return (
    <>
      <PageHeader
        title="后台任务"
        action={
          admin && (
            <Button variant="outline" onClick={() => void mutate()}>
              <RefreshCw className="size-4" />
              刷新
            </Button>
          )
        }
      />
      {!admin ? (
        <Panel>
          <Empty
            title="需要管理员权限"
          />
        </Panel>
      ) : (
        <>
          {status === "failed" && !!data?.items?.length && (
            <div className="mb-4 flex justify-end">
              <Button variant="outline" size="sm" disabled={busy} onClick={() => void setIgnored(data.items ?? [], true)}>
                <CheckCircle2 className="size-4" />忽略本页提醒
              </Button>
            </div>
          )}
          <Panel
            title="后台任务列表"
            action={
              <Select
                aria-label="后台任务状态"
                className="w-40"
                value={status}
                onChange={(e) => {
                  setStatus(e.target.value);
                  setCursor("");
                }}
              >
                <option value="failed">失败待处理</option>
                <option value="ignored">已忽略</option>
                <option value="pending">等待执行</option>
                <option value="running">运行中</option>
                <option value="succeeded">成功</option>
                <option value="">全部状态</option>
              </Select>
            }
          >
            {isLoading ? (
              <Loading />
            ) : error ? (
              <ErrorState error={error} retry={() => void mutate()} />
            ) : data?.items?.length ? (
              <div className="overflow-x-auto">
                <table className="w-full text-left text-sm">
                  <thead className="border-b border-border bg-muted/40 text-muted-foreground">
                    <tr>
                      {[
                        "任务",
                        "状态",
                        "执行次数",
                        "最近错误",
                        "更新时间",
                        "操作",
                      ].map((h) => (
                        <th key={h} className="px-5 py-3 font-medium">
                          {h}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border">
                    {data.items.map((job) => (
                      <tr key={job.id} className="table-row">
                        <td className="px-5 py-4">
                          <button
                            className="text-left font-medium hover:text-primary"
                            onClick={() => setDetailId(job.id ?? "")}
                          >
                            {jobName(job)}
                            <span className="mt-1 block font-mono text-[10px] font-normal text-muted-foreground">
                              {shortId(job.id)}
                            </span>
                          </button>
                        </td>
                        <td className="px-5 py-4">
                          <Status value={job.ignoredAt ? "ignored" : job.status} />
                        </td>
                        <td className="px-5 py-4 font-mono">
                          {job.attempts ?? 0}
                        </td>
                        <td className="max-w-xs px-5 py-4">
                          <p className="line-clamp-2 break-words text-muted-foreground">
                            {job.lastError || "—"}
                          </p>
                        </td>
                        <td className="px-5 py-4">
                          <Time value={job.updatedAt} />
                        </td>
                        <td className="px-5 py-4">
                          <div className="flex gap-1">
                            <Button
                              variant="ghost"
                              size="sm"
                              onClick={() => setDetailId(job.id ?? "")}
                            >
                              <ArrowRight className="size-3.5" />
                              详情
                            </Button>
                            {job.status === "failed" && (
                              <Button size="sm" variant="ghost" disabled={busy} onClick={() => void setIgnored([job], !job.ignoredAt)}>
                                {job.ignoredAt ? "恢复提醒" : "忽略提醒"}
                              </Button>
                            )}
                            {job.status === "failed" && (
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() => {
                                  setRetry(job);
                                  setReason("");
                                }}
                              >
                                <RefreshCw className="size-3.5" />
                                重试
                              </Button>
                            )}
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <Empty
                title={
                  status === "failed" ? "暂无待处理的失败任务" : status === "ignored" ? "暂无已忽略的任务" : "暂无后台任务"
                }
              />
            )}
            <div className="flex items-center justify-between border-t border-border px-5 py-3">
              <span className="text-xs text-muted-foreground">
                共 {data?.total ?? "—"} 项任务
              </span>
              <div className="flex gap-2">
                {cursor && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setCursor("")}
                  >
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
        </>
      )}
      <Dialog
        open={!!detailId}
        onOpenChange={(open) => {
          if (!open) setDetailId("");
        }}
        title={detail ? jobName(detail) : "后台任务详情"}
      >
        {detailLoading ? (
          <Loading />
        ) : detailError ? (
          <ErrorState error={detailError} retry={() => void refreshDetail()} />
        ) : (
          detail && (
            <div className="space-y-5">
              <div>
                <Definition
                  label="任务 ID"
                  value={
                    <span className="font-mono break-all">{detail.id}</span>
                  }
                />
                <Definition label="任务类型" value={detail.kind} />
                <Definition
                  label="状态"
                  value={<Status value={detail.ignoredAt ? "ignored" : detail.status} />}
                />
                <Definition label="执行次数" value={detail.attempts} />
                <Definition
                  label="最早执行时间"
                  value={<Time value={detail.availableAt} />}
                />
                <Definition
                  label="创建时间"
                  value={<Time value={detail.createdAt} />}
                />
              </div>
              {detail.status === "failed" && (
                <div className="flex justify-end">
                  <Button variant="outline" size="sm" disabled={busy} onClick={() => void setIgnored([detail], !detail.ignoredAt)}>{detail.ignoredAt ? "恢复提醒" : "忽略失败提醒"}</Button>
                </div>
              )}
              {detail.lastError && (
                <p className="rounded-lg bg-red-50 p-4 text-xs leading-6 break-words text-red-700">
                  {detail.lastError}
                </p>
              )}
              <details className="rounded-lg border border-border p-3">
                <summary className="cursor-pointer text-xs text-muted-foreground">
                  任务参数
                </summary>
                <pre className="mt-3 overflow-auto font-mono text-xs whitespace-pre-wrap break-words">
                  {JSON.stringify(objectData(detail.payload), null, 2)}
                </pre>
              </details>
            </div>
          )
        )}
      </Dialog>
      <Dialog
        open={!!retry}
        onOpenChange={(open) => {
          if (!open) setRetry(null);
        }}
        title="重试失败任务"
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            if (!retry?.id || !reason.trim()) return;
            setBusy(true);
            try {
              await api.retryJob(retry.id, {
                expectedAttempts: retry.attempts ?? 0,
                reason: reason.trim(),
              });
              toast.success("任务已重新排队");
              setRetry(null);
              await Promise.all([mutate(), mutateCache("dashboard")]);
              if (detailId) await refreshDetail();
            } catch (err) {
              reportError(err);
              await mutate();
            } finally {
              setBusy(false);
            }
          }}
        >
          {retry && (
            <p className="text-sm">
              {jobName(retry)} · 已执行 {retry.attempts ?? 0} 次
            </p>
          )}
          <Field label="重试原因">
            <Textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required
              maxLength={2000}
              placeholder="例如：模型服务已恢复，重新入队执行"
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setRetry(null)}
            >
              取消
            </Button>
            <Button type="submit" disabled={busy || !reason.trim()}>
              {busy && <Loader2 className="size-4 animate-spin" />}重试任务
            </Button>
          </div>
        </form>
      </Dialog>
    </>
  );
}
