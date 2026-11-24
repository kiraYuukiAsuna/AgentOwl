"use client";
import { useState } from "react";
import { Check, Loader2, MessageCircle, UserCheck } from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api/client";
import type { CaseEscaped } from "@/lib/api/generated/models";
import { Panel, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Field, Textarea } from "@/components/ui/input";

const choices = [
  {
    action: "resolved",
    title: "已解决",
    icon: Check,
    description: "",
    success: "已标记为已解决",
  },
  {
    action: "unresolved",
    title: "未解决",
    icon: MessageCircle,
    description: "提交后，问题将重新回到“处理中”状态。",
    success: "已标记为未解决",
  },
  {
    action: "human",
    title: "转人工",
    icon: UserCheck,
    description: "提交后将暂停自动回复，交由人工跟进。",
    success: "已转人工",
  },
] as const;

export function CaseFeedback({
  caseId,
  version,
  onSaved,
}: {
  caseId: string;
  version: number;
  onSaved: (next?: CaseEscaped) => Promise<unknown>;
}) {
  const [choice, setChoice] = useState<(typeof choices)[number] | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState<null | (typeof choices)[number]["action"]>(null);
  const locked = busy !== null;
  return (
    <>
      <Panel
        title="用户反馈"
      >
        <div className="flex flex-wrap gap-2 p-5">
          {choices.map((item) => {
            const active = busy === item.action;
            return (
              <Button
                key={item.action}
                variant={active ? "default" : "outline"}
                size="sm"
                disabled={locked}
                onClick={() => {
                  setChoice(item);
                  setReason("");
                }}
              >
                {busy === item.action ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <item.icon className="size-3.5" />
                )}
                {item.title}
              </Button>
            );
          })}
        </div>
      </Panel>
      <Dialog
        open={choice !== null}
        onOpenChange={(open) => {
          if (open || busy) return;
          setChoice(null);
          setReason("");
        }}
        title={choice?.title ?? "用户反馈"}
        description={choice?.description}
      >
        {choice && (
          <form
            className="space-y-5"
            onSubmit={async (event) => {
              event.preventDefault();
              if (!choice || !reason.trim()) return;
              setBusy(choice.action);
              try {
                const next = await api.caseFeedback(caseId, {
                  action: choice.action,
                  expectedVersion: version,
                  reason: reason.trim(),
                });
                setChoice(null);
                setReason("");
                toast.success(choice.success);
                await onSaved(next);
              } catch (error) {
                reportError(error);
              } finally {
                setBusy(null);
              }
            }}
          >
            <Field
              label="反馈说明"
            >
              <Textarea
                value={reason}
                onChange={(event) => setReason(event.target.value)}
                required
                maxLength={2000}
                placeholder="补充具体情况或原因（请勿包含密码或 Token）"
              />
            </Field>
            <Button type="submit" disabled={busy !== null || !reason.trim()}>
              {busy === choice?.action && <Loader2 className="size-3.5 animate-spin" />}
              提交反馈
            </Button>
          </form>
        )}
      </Dialog>
    </>
  );
}
