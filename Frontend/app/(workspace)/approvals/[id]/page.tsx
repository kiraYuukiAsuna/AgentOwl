"use client";
import { useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import useSWR from "swr";
import { Check, Loader2, X } from "lucide-react";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { approvalTarget } from "@/lib/workflow";
import { objectData } from "@/lib/utils";
import { api, useMe, runtimeWrite } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Definition,
  Status,
  Time,
  reportError,
  label,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Textarea, Field } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import { ArtifactLinks } from "@/components/artifact-links";
import { ApprovalEvidence } from "@/components/approval-evidence";
export default function ApprovalPage() {
  const { id } = useParams<{ id: string }>();
  const { data: me } = useMe();
  const {
    data: item,
    error,
    isLoading,
    mutate,
  } = useSWR(["approval", id], () => api.approval(id), {
    refreshInterval: 10000,
  });
  const [decision, setDecision] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const canApprove =
    item?.subjectType === "knowledge"
      ? hasRole(me, "knowledge")
      : item?.subjectType === "reply"
        ? hasRole(me, "developer", "oncall")
        : ["step", "admission"].includes(item?.subjectType ?? "") && hasRole(me, "developer");
  async function decide(e: React.FormEvent) {
    e.preventDefault();
    if (!item) return;
    if (decision !== "approve" && !reason.trim()) {
      toast.error(decision === "revoke" ? "请填写撤销原因" : "请填写拒绝原因");
      return;
    }
    setBusy(true);
    try {
      await api.decide(id, {
        decision,
        reason: reason.trim(),
        expectedVersion: item.version ?? 0,
      });
      toast.success("审批结果已保存");
      setDecision("");
      setReason("");
      await mutate();
    } catch (err) {
      reportError(err);
      await mutate();
    } finally {
      setBusy(false);
    }
  }
  async function regenerate() {
    if (!item?.subjectId) return;
    setBusy(true);
    try { await runtimeWrite(`/api/v1/admission-decisions/${item.subjectId}/actions`, "POST", { action: "reevaluate" }); toast.success("已提交重新生成，请稍后刷新查看"); await mutate(); }
    catch (error) { reportError(error); }
    finally { setBusy(false); }
  }
  if (isLoading) return <Loading />;
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!item) return null;
  const target = approvalTarget(item);
  const evidence = objectData(item.action);
  const evidenceInput =
    evidence.input &&
    typeof evidence.input === "object" &&
    !Array.isArray(evidence.input)
      ? (evidence.input as Record<string, unknown>)
      : {};
  return (
    <>
      <PageHeader
        back="/approvals"
        eyebrow="审批详情"
        title={item.summary || label(item.kind)}
      />
      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_300px]">
        <div className="space-y-6">
          <Panel title="审批内容" action={<Status value={item.status} />}>
            <div className="p-5">
              <ApprovalEvidence approval={item} />
              <div className="mt-4">
                <ArtifactLinks data={{ ...evidenceInput, ...evidence }} />
              </div>
              <details className="mt-6 rounded-lg border border-border px-4 py-3">
                <summary className="cursor-pointer text-xs font-medium text-muted-foreground">
                  详细参数（JSON）
                </summary>
                <pre className="mt-4 overflow-x-auto rounded-lg bg-muted/40 p-4 font-mono text-xs leading-7 break-words whitespace-pre-wrap">
                  {JSON.stringify(objectData(item.action), null, 2)}
                </pre>
                <p className="mt-4 break-all font-mono text-[10px] text-muted-foreground">
                  内容摘要（SHA-256）：{item.actionHash || "未提供"}
                </p>
              </details>
            </div>
          </Panel>
          {item.reason && (
            <Panel title="审批说明">
              <p className="p-5 prose-body">{item.reason}</p>
            </Panel>
          )}
        </div>
        <aside className="space-y-5">
          <Panel title="审批信息">
            <div className="px-5 py-3">
              <Definition label="操作" value={label(item.kind)} />
              <Definition label="状态" value={<Status value={item.status} />} />
              <Definition
                label="提交时间"
                value={<Time value={item.createdAt} />}
              />
              <Definition
                label="有效期至"
                value={<Time value={item.expiresAt} />}
              />
              {target && (
                <Definition
                  label="关联记录"
                  value={
                    <Link
                      className="text-primary hover:underline"
                      href={target}
                    >
                      查看详情 →
                    </Link>
                  }
                />
              )}
            </div>
          </Panel>
          {canApprove && item.kind === "create_task" && ["blocked", "stale", "expired", "rejected"].includes(item.status ?? "") && <Panel title="继续处理"><div className="space-y-3 p-5">
            {target && <Link className="block text-sm text-primary hover:underline" href={target}>前往问题页补充信息 →</Link>}
            <Button disabled={busy} onClick={() => void regenerate()}>重新生成任务建议</Button>
          </div></Panel>}
          {canApprove && item.status === "pending" && (
            <Panel title="处理审批">
              <div className="space-y-3 p-5">
                <Button
                  className="w-full"
                  onClick={() => setDecision("approve")}
                >
                  <Check className="size-4" />
                  批准
                </Button>
                <Button
                  variant="outline"
                  className="w-full"
                  onClick={() => setDecision("reject")}
                >
                  <X className="size-4" />
                  拒绝
                </Button>
              </div>
            </Panel>
          )}
          {canApprove && item.status === "approved" && (
            <Button
              variant="destructive"
              className="w-full"
              onClick={() => setDecision("revoke")}
            >
              撤销批准
            </Button>
          )}
        </aside>
      </div>
      <Dialog
        open={!!decision}
        onOpenChange={(o) => {
          if (!o) setDecision("");
        }}
        title={
          decision === "approve"
            ? "确认批准"
            : decision === "revoke"
              ? "撤销批准"
              : "拒绝"
        }
        description={decision === "revoke" ? "撤销仅阻止尚未执行的操作；已发出的消息或已提交的 PR 无法撤回。" : item.summary || label(item.kind)}
      >
        <form onSubmit={decide} className="space-y-5">
          <Field
            label={decision === "approve" ? "审批备注（可选）" : decision === "revoke" ? "撤销原因" : "拒绝原因"}
          >
            <Textarea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              required={decision !== "approve"}
              placeholder={decision === "approve" ? "可补充审批备注" : decision === "revoke" ? "说明撤销原因" : "说明拒绝原因或需要调整的地方"}
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button
              variant="ghost"
              type="button"
              onClick={() => setDecision("")}
            >
              取消
            </Button>
            <Button
              variant={decision === "approve" ? "default" : "destructive"}
              type="submit"
              disabled={busy}
            >
              {busy && <Loader2 className="size-4 animate-spin" />}{decision === "approve" ? "确认批准" : decision === "revoke" ? "确认撤销" : "确认拒绝"}
            </Button>
          </div>
        </form>
      </Dialog>
    </>
  );
}
