"use client";
import { useState } from "react";
import { useParams } from "next/navigation";
import useSWR from "swr";
import {
  FilePenLine,
  History,
  Loader2,
  Send,
  Shield,
  Undo2,
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
  SafeExternalLink,
  label,
  reportError,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Textarea, Field } from "@/components/ui/input";
import { KnowledgeEditor } from "@/components/knowledge-editor";
export default function KnowledgeDetail() {
  const { id } = useParams<{ id: string }>();
  const { data: me } = useMe();
  const {
    data: item,
    error,
    isLoading,
    mutate,
  } = useSWR(["knowledge-detail", id], () => api.knowledgeItem(id));
  const { data: history, mutate: refreshHistory } = useSWR(
    ["knowledge-versions", id],
    () => api.knowledgeVersions(id),
  );
  const [edit, setEdit] = useState(false);
  const [withdraw, setWithdraw] = useState(false);
  const [busy, setBusy] = useState(false);
  const canWrite = hasRole(me, "knowledge", "developer");
  if (isLoading) return <Loading />;
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!item) return null;
  return (
    <>
      <PageHeader
        back="/knowledge"
        eyebrow="知识库"
        title={item.title || "知识详情"}
        description={item.module || "通用知识"}
        action={
          canWrite && (
            <>
              <Button variant="outline" onClick={() => setEdit(true)}>
                <FilePenLine className="size-4" />
                创建新版本
              </Button>
              {item.status === "draft" && (
                <Button
                  disabled={busy}
                  onClick={async () => {
                    setBusy(true);
                    try {
                      await api.submitKnowledge(id, {
                        expectedVersion: item.version ?? 0,
                      });
                      toast.success("已提交发布审批");
                      await mutate();
                    } catch (e) {
                      reportError(e);
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  {busy ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <Send className="size-4" />
                  )}
                  提交发布审批
                </Button>
              )}
            </>
          )
        }
      />
      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_290px]">
        <Panel>
          <div className="flex items-center justify-between border-b border-border px-6 py-4">
            <Status value={item.status} />
            <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
              <Shield className="size-3.5" />
              {label(item.visibility)}
            </span>
          </div>
          <article className="p-6 prose-body md:p-8">{item.body}</article>
        </Panel>
        <aside className="space-y-5">
          <Panel title="适用范围">
            <div className="px-5 py-3">
              <Definition label="模块" value={item.module} />
              <Definition label="CLI 版本" value={item.cliVersion} />
              <Definition label="环境" value={item.environment} />
              <Definition
                label="有效至"
                value={<Time value={item.validUntil} />}
              />
              <Definition
                label="更新于"
                value={<Time value={item.updatedAt} />}
              />
              <Definition
                label="来源"
                value={
                  <SafeExternalLink href={item.sourceUrl}>
                    查看原文
                  </SafeExternalLink>
                }
              />
            </div>
          </Panel>
          <Panel
            title="版本记录"
            action={<History className="size-3.5 text-muted-foreground" />}
          >
            <div className="divide-y divide-border">
              {history?.items?.map((v, i) => (
                <div
                  key={`${v.id}-${v.version}-${i}`}
                  className="flex items-center justify-between px-5 py-3"
                >
                  <div>
                    <Time value={v.createdAt} />
                  </div>
                  <Status value={v.status} />
                </div>
              )) ?? (
                <p className="p-5 text-xs text-muted-foreground">
                  正在加载版本记录…
                </p>
              )}
            </div>
          </Panel>
          {canWrite && item.status === "published" && (
            <Button
              variant="destructive"
              className="w-full"
              onClick={() => setWithdraw(true)}
            >
              <Undo2 className="size-4" />
              撤回知识
            </Button>
          )}
        </aside>
      </div>
      <Dialog
        open={edit}
        onOpenChange={setEdit}
        title="创建新版本"
        description="新版本需重新审批。"
      >
        <KnowledgeEditor
          value={item}
          onCancel={() => setEdit(false)}
          onSave={async (value) => {
            await api.newKnowledgeVersion(id, value);
            toast.success("新版本已保存");
            setEdit(false);
            await Promise.all([mutate(), refreshHistory()]);
          }}
        />
      </Dialog>
      <Dialog
        open={withdraw}
        onOpenChange={setWithdraw}
        title="撤回知识"
        description="撤回后不再用于新回复，历史回复和引用会保留。"
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            const form = new FormData(e.currentTarget);
            setBusy(true);
            try {
              await api.withdrawKnowledge(id, {
                expectedVersion: item.version ?? 0,
                reason: String(form.get("reason")),
              });
              toast.success("知识已撤回");
              setWithdraw(false);
              await mutate();
            } catch (err) {
              reportError(err);
            } finally {
              setBusy(false);
            }
          }}
        >
          <Field label="撤回原因">
            <Textarea name="reason" required />
          </Field>
          <Button type="submit" variant="destructive" disabled={busy}>
            确认撤回
          </Button>
        </form>
      </Dialog>
    </>
  );
}
