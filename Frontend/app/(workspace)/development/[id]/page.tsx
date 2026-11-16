"use client";
import { TaskInputs } from "@/components/task-inputs";
import { useState } from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import useSWR from "swr";
import {
  Check,
  ChevronDown,
  Code2,
  FileCode2,
  GitPullRequest,
  Loader2,
  Pause,
  Play,
  Square,
  Terminal,
  UserRoundCheck,
} from "lucide-react";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  Loading,
  ErrorState,
  Status,
  Time,
  Definition,
  Empty,
  SafeExternalLink,
  label,
  reportError,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Textarea, Field, Select } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import { ArtifactLinks } from "@/components/artifact-links";
import { cn, shortId, objectData } from "@/lib/utils";
const stages = ["plan", "edit", "test", "review", "publish_pr", "track"];
export default function TaskDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { data: me } = useMe();
  const {
    data: task,
    error,
    isLoading,
    mutate,
  } = useSWR(["task", id], () => api.task(id), { refreshInterval: 5000 });
  const [control, setControl] = useState("");
  const [busy, setBusy] = useState(false);
  const [runId, setRunId] = useState("");
  const [revisionOpen, setRevisionOpen] = useState(false);
  const [logCursor, setLogCursor] = useState("");
  const selectedRun = runId || task?.runs?.[0]?.id;
  const { data: logs, error: logError } = useSWR(
    selectedRun ? ["run-events", selectedRun, logCursor] : null,
    () => api.runEvents(selectedRun ?? "", { limit: 100, cursor: logCursor }),
    { refreshInterval: 5000 },
  );
  const canWrite = hasRole(me, "developer");
  if (isLoading) return <Loading />;
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!task) return null;
  const current = task.status === "completed" ? stages.length : stages.indexOf(task.stage ?? "");
  const terminal = ["completed", "cancelled"].includes(task.status ?? "");
  const resumable = ["paused", "blocked", "failed"].includes(task.status ?? "");
  return (
    <>
      <PageHeader
        back="/development"
        eyebrow={`开发任务 ${shortId(task.id)}`}
        title={task.title || task.goal || "开发任务"}
        description={`版本 ${task.revision ?? 1} · ${task.status === "completed" ? "已完成" : `当前阶段：${label(task.stage)}`}`}
        action={
          canWrite &&
          !terminal && (
            <>
              <Button
                variant="outline"
                onClick={() => setControl(resumable ? "resume" : "pause")}
              >
                {resumable ? (
                  <Play className="size-4" />
                ) : (
                  <Pause className="size-4" />
                )}
                {resumable ? "恢复" : "暂停"}
              </Button>
              <Button variant="outline" onClick={() => setControl("takeover")}>
                <UserRoundCheck className="size-4" />
                人工接管
              </Button>
              <Button
                variant="destructive"
                onClick={() => setControl("cancel")}
              >
                <Square className="size-3.5" />
                取消任务
              </Button>
            </>
          )
        }
      />
      <TaskInputs id={id} goal={task.goal ?? ""} acceptance={task.acceptanceCriteria ?? ""} writable={canWrite && !terminal} refresh={mutate} />
      <div className="mb-6 grid grid-cols-3 overflow-hidden rounded-xl border border-border bg-white md:grid-cols-6">
        {stages.map((s, i) => (
          <div
            key={s}
            className={cn(
              "relative flex items-center gap-2.5 border-r border-border px-4 py-4 last:border-0",
              i === current && "bg-primary/7",
            )}
          >
            <span
              className={cn(
                "flex size-6 shrink-0 items-center justify-center rounded-full border text-[10px]",
                i < current
                  ? "border-primary bg-primary text-white"
                  : i === current
                    ? "border-primary/30 text-primary"
                    : "border-border text-muted-foreground",
              )}
            >
              {i < current ? <Check className="size-3" /> : i + 1}
            </span>
            <span
              className={cn(
                "text-xs",
                i === current
                  ? "font-semibold text-primary"
                  : "text-muted-foreground",
              )}
            >
              {label(s)}
            </span>
          </div>
        ))}
      </div>
      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_290px]">
        <div className="space-y-6">
          {task.errorEscaped && (
            <div
              role="alert"
              className="rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm leading-relaxed text-amber-800"
            >
              {task.errorEscaped}
            </div>
          )}
          <Panel title="开发目标">
            <div className="space-y-5 p-5">
              <div>
                <h3 className="mb-2 text-xs font-medium text-muted-foreground">
                  预期结果
                </h3>
                <p className="prose-body">{task.goal}</p>
              </div>
              <div>
                <h3 className="mb-2 text-xs font-medium text-muted-foreground">
                  验收条件
                </h3>
                <p className="prose-body">{task.acceptanceCriteria}</p>
              </div>
            </div>
          </Panel>
          <Panel title="阶段结果">
            {task.steps?.length ? (
              <div className="divide-y divide-border">
                {task.steps.map((s) => (
                  <details
                    key={s.id}
                    className="group px-5 py-4"
                  >
                    <summary className="flex cursor-pointer list-none items-center gap-3">
                      <FileCode2 className="size-4 text-muted-foreground" />
                      <span className="flex-1 text-sm font-medium">
                        {label(s.stage)}
                      </span>
                      <Status value={s.status} />
                      <ChevronDown className="size-4 text-muted-foreground transition group-open:rotate-180" />
                    </summary>
                    <div className="mt-4 space-y-3 pl-7">
                      {s.errorEscaped && (
                        <p className="text-xs text-red-700">{s.errorEscaped}</p>
                      )}
                      {s.approvalId && (
                        <Link
                          className="inline-block text-xs text-primary hover:underline"
                          href={`/approvals/${s.approvalId}`}
                        >
                          查看本阶段审批 →
                        </Link>
                      )}
                      <pre className="overflow-auto rounded-lg bg-muted p-3 font-mono text-xs leading-6 whitespace-pre-wrap break-words">
                        {JSON.stringify(
                          Object.keys(objectData(s.output)).length
                            ? objectData(s.output)
                            : objectData(s.input),
                          null,
                          2,
                        )}
                      </pre>
                      <ArtifactLinks
                        data={{
                          ...objectData(s.input),
                          ...objectData(s.output),
                        }}
                      />
                    </div>
                  </details>
                ))}
              </div>
            ) : (
              <Empty
                title="暂无执行记录"
              />
            )}
          </Panel>
          <Panel
            title="运行日志"
            action={<Terminal className="size-4 text-muted-foreground" />}
          >
            <div className="flex flex-wrap gap-2 border-b border-border p-3">
              {task.runs?.map((r) => (
                <button
                  key={r.id}
                  onClick={() => {
                    setRunId(r.id ?? "");
                    setLogCursor("");
                  }}
                  className={cn(
                    "rounded-md border px-2.5 py-1.5 text-[11px]",
                    selectedRun === r.id
                      ? "border-primary/30 bg-primary/5 text-primary"
                      : "border-border text-muted-foreground",
                  )}
                >
                  {label(r.stage)} · {shortId(r.id)}
                </button>
              ))}
            </div>
            {logError ? (
              <ErrorState error={logError} />
            ) : logs?.items?.length ? (
              <div className="max-h-[440px] overflow-auto bg-slate-950 px-5 py-4 font-mono text-[11px] leading-6 text-slate-200">
                {logs.items.map((e, i) => (
                  <div
                    key={e.id ?? i}
                    className="break-all whitespace-pre-wrap"
                  >
                    <span className="mr-3 text-slate-500">
                      {e.createdAt?.replace("T", " ").slice(11, 19) || ""}
                    </span>
                    {e.body || e.type}
                  </div>
                ))}
              </div>
            ) : (
              <Empty
                title="暂无日志"
              />
            )}
            {(logCursor || logs?.nextCursor) && (
              <div className="flex justify-end gap-2 border-t border-border p-3">
                {logCursor && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setLogCursor("")}
                  >
                    返回第一页
                  </Button>
                )}
                <Button
                  size="sm"
                  variant="outline"
                  disabled={!logs?.nextCursor}
                  onClick={() => setLogCursor(logs?.nextCursor ?? "")}
                >
                  下一页
                </Button>
              </div>
            )}
          </Panel>
        </div>
        <aside className="space-y-5">
          <Panel title="任务信息">
            <div className="px-5 py-3">
              <Definition label="状态" value={<Status value={task.status} />} />
              <Definition label="当前运行" value={task.epoch} />
              <Definition
                label="创建时间"
                value={<Time value={task.createdAt} />}
              />
              <Definition
                label="起始提交"
                value={
                  <span className="font-mono">{shortId(task.baseCommit)}</span>
                }
              />
              <Definition
                label="当前提交"
                value={
                  <span className="font-mono">{shortId(task.headCommit)}</span>
                }
              />
              <Definition
                label="关联问题"
                value={
                  <Link className="text-primary" href={`/cases/${task.caseId}`}>
                    查看问题 →
                  </Link>
                }
              />
            </div>
          </Panel>
          {task.prUrl && (
            <Panel title="PR">
              <div className="p-5 text-sm">
                <SafeExternalLink href={task.prUrl}>
                  <GitPullRequest className="size-4" />
                  查看 GitHub PR
                </SafeExternalLink>
              </div>
            </Panel>
          )}
          {canWrite && !terminal && (
            <Button
              variant="outline"
              className="w-full"
              onClick={() => setRevisionOpen(true)}
            >
              <Code2 className="size-4" />
              修改目标与验收条件
            </Button>
          )}
        </aside>
      </div>
      <Dialog
        open={!!control}
        onOpenChange={(o) => {
          if (!o) setControl("");
        }}
        title={
          control === "cancel"
            ? "取消开发任务"
            : control === "pause"
              ? "暂停开发任务"
              : control === "takeover"
                ? "人工接管开发任务"
                : "恢复开发任务"
        }
        description={control === "cancel" ? "取消后无法恢复，已发送的消息和 PR 不会撤回。" : control === "resume" ? "恢复前会重新检查审批。" : "当前运行将停止，恢复时会重新检查审批。"}
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            const f = new FormData(e.currentTarget);
            setBusy(true);
            try {
              await api.taskAction(id, {
                action: control,
                expectedVersion: task.version ?? 0,
                reason: String(f.get("reason") || ""),
              });
              toast.success("操作已提交");
              setControl("");
              await mutate();
            } catch (err) {
              reportError(err);
            } finally {
              setBusy(false);
            }
          }}
        >
          <Field label="操作原因">
            <Textarea
              name="reason"
              required={control === "cancel"}
              placeholder="填写操作原因"
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setControl("")}
            >
              返回
            </Button>
            <Button
              type="submit"
              disabled={busy}
              variant={control === "cancel" ? "destructive" : "default"}
            >
              {busy && <Loader2 className="size-4 animate-spin" />}确认
            </Button>
          </div>
        </form>
      </Dialog>
      <Dialog
        open={revisionOpen}
        onOpenChange={setRevisionOpen}
        title="修改目标与验收条件"
        description="保存为新版本后，需重新审批和测试。"
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            const f = new FormData(e.currentTarget);
            setBusy(true);
            try {
              await api.reviseTask(id, {
                repositoryId: task.repositoryId ?? "",
                goal: String(f.get("goal")),
                acceptanceCriteria: String(f.get("acceptanceCriteria")),
                followupMode: f.get("followupMode") === "within_scope" ? "within_scope" : "manual",
                expectedVersion: task.version ?? 0,
              });
              toast.success("修改已保存");
              setRevisionOpen(false);
              await mutate();
            } catch (err) {
              reportError(err);
            } finally {
              setBusy(false);
            }
          }}
        >
          <Field label="预期结果">
            <Textarea name="goal" defaultValue={task.goal ?? ""} required />
          </Field>
          <Field label="验收条件">
            <Textarea
              name="acceptanceCriteria"
              defaultValue={task.acceptanceCriteria ?? ""}
              required
            />
          </Field>
          <Field label="后续补充"><Select name="followupMode" defaultValue="manual"><option value="manual">由我确认后应用</option><option value="within_scope">自动应用范围内补充</option></Select></Field>
          <Button type="submit" disabled={busy}>
            保存修改
          </Button>
        </form>
      </Dialog>
    </>
  );
}
