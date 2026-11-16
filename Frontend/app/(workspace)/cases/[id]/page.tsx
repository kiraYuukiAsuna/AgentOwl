"use client";
import { activityAction, caseEventCategory, caseSourceLabel } from "@/lib/activity-labels";
import { useState } from "react";
import { useParams, useRouter } from "next/navigation";
import Link from "next/link";
import useSWR from "swr";
import {
  ArrowRight,
  Bot,
  BookOpen,
  Check,
  Code2,
  FilePenLine,
  GitBranch,
  Loader2,
  Pause,
  Play,
  RefreshCw,
  Send,
  Sparkles,
  UserCheck,
} from "lucide-react";
import { toast } from "sonner";
import { z } from "zod";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  Loading,
  ErrorState,
  Status,
  Priority,
  Time,
  Definition,
  Empty,
  reportError,
  label,
  SafeExternalLink,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Input, Textarea, Select, Field } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import { SearchSelect } from "@/components/search-select";
import { cn, shortId, objectData } from "@/lib/utils";
import { KnowledgeEditor } from "@/components/knowledge-editor";
import { CaseAttachments } from "@/components/case-attachments";
import { CaseFeedback } from "@/components/case-feedback";
import { ActivityMarker, activityDotClass } from "@/components/activity-marker";
import type { Knowledge } from "@/lib/api/generated/models";

export default function CaseDetailPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const { data: me } = useMe();
  const {
    data: item,
    error,
    isLoading,
    mutate,
  } = useSWR(["case", id], () => api.case(id), { refreshInterval: 12000 });
  const { data: people } = useSWR("principals", () =>
    api.principals({ limit: 100 }),
  );
  const { data: repos } = useSWR("repositories", () =>
    api.repositories({ limit: 100 }),
  );
  const { data: channels } = useSWR("channels-options", () =>
    api.channels({ limit: 100 }),
  );
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState("");
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [repository, setRepository] = useState("");
  const [relationOpen, setRelationOpen] = useState(false);
  const [editOpen, setEditOpen] = useState(false);
  const [knowledgeDraft, setKnowledgeDraft] = useState<Knowledge | null>(null);
  const canWrite = hasRole(me, "oncall", "developer");
  const canDevelop = hasRole(me, "developer");
  async function action(name: string) {
    if (!item) return;
    setBusy(name);
    try {
      await api.caseAction(id, {
        action: name,
        expectedVersion: item.version ?? 0,
      });
      toast.success(name === "retry_classification" ? "已提交重新分类" : "问题状态已更新");
      await mutate();
    } catch (e) {
      reportError(e);
    } finally {
      setBusy("");
    }
  }
  async function saveReply(generate = false) {
    if (!item) return;
    if (!generate && !body.trim()) {
      toast.error("请填写回复内容");
      return;
    }
    setBusy(generate ? "generate" : "reply");
    try {
      await api.reply(id, {
        body: body.trim(),
        generate,
        expectedVersion: item.version ?? 0,
      });
      toast.success(
        generate ? "正在生成草稿" : "回复草稿已保存",
      );
      setBody("");
      await mutate();
    } catch (e) {
      reportError(e);
    } finally {
      setBusy("");
    }
  }
  async function confirm(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (!item) return;
    const data = Object.fromEntries(new FormData(e.currentTarget));
    const parsed = z
      .object({
        goal: z.string().trim().min(1, "请填写修改目标"),
        acceptanceCriteria: z.string().trim().min(1, "请填写验收条件"),
        repositoryId: z.string().min(1, "请选择仓库"),
      })
      .safeParse({ ...data, repositoryId: repository });
    if (!parsed.success) {
      toast.error(parsed.error.issues[0].message);
      return;
    }
    setBusy("confirm");
    try {
      const task = await api.confirmDevelopment(id, {
        ...parsed.data,
        followupMode: data.followupMode === "within_scope" ? "within_scope" : "manual",
        expectedVersion: item.version ?? 0,
      });
      setConfirmOpen(false);
      toast.success("开发任务已创建");
      await mutate();
      if (task?.id) router.push(`/development/${task.id}`);
    } catch (e) {
      reportError(e);
    } finally {
      setBusy("");
    }
  }
  if (isLoading) return <Loading />;
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!item) return null;
  const lastClassification = item.events?.find((event) =>
    event.type === "classified" ||
    (event.type === "needs_human" && ["分类服务暂不可用，已转交人工判断", "模型配置暂不可用，已转交人工判断", "尚未配置模型，已转交人工判断", "模型预算不可用，已转交人工判断"].some((text) => event.body?.startsWith(text))),
  );
  return (
    <>
      <PageHeader
        back="/cases"
        eyebrow={`OWL-${shortId(id)}`}
        title={item.title || "问题详情"}
        description={item.summary || undefined}
        action={
          canWrite && (
            <>
              <Button
                variant="outline"
                disabled={!!busy}
                onClick={() => void action("claim")}
              >
                <UserCheck className="size-4" />
                认领
              </Button>
              {canDevelop && !item.developmentTaskId && (
                <Button onClick={() => setConfirmOpen(true)}>
                  <Code2 className="size-4" />
                  创建开发任务
                </Button>
              )}
            </>
          )
        }
      />
      <div className="mb-6 flex flex-wrap items-center gap-3">
        <Status value={item.status} />
        <Priority value={item.priority} />
        <span className="text-xs text-muted-foreground">
          {item.module || "未分类"} / {label(item.category || "unknown")}
        </span>
        <span className="text-border">·</span>
        <Time value={item.createdAt} />
      </div>
      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_290px]">
        <div className="space-y-6">
          <Panel
            title="问题描述"
            action={
              canWrite && (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setEditOpen(true)}
                >
                  <FilePenLine className="size-3.5" />
                  编辑
                </Button>
              )
            }
          >
            <div className="p-5 prose-body">
              {item.description || "暂无描述"}
            </div>
            {Object.keys(objectData(item.environment)).length > 0 && (
              <div className="border-t border-border px-5 py-4">
                <p className="mb-3 text-xs font-medium">环境信息</p>
                <pre className="overflow-auto rounded-lg bg-muted p-3 font-mono text-xs leading-6">
                  {JSON.stringify(objectData(item.environment), null, 2)}
                </pre>
              </div>
            )}
          </Panel>
          <CaseAttachments
            caseId={id}
            version={item.version ?? 0}
            references={objectData(item.environment).attachments}
          />
          <Panel
            title="原始对话"
          >
            {item.messages?.length ? (
              <div className="divide-y divide-border">
                {item.messages.map((m) => (
                  <div key={m.id} className="p-5">
                    <div className="mb-3 flex items-center gap-2">
                      <span className="flex size-7 items-center justify-center rounded-full bg-muted text-[10px]">
                        {(m.actorName || m.actorId || "用户").slice(0, 1).toUpperCase()}
                      </span>
                      <span className="text-sm font-medium">
                        {m.actorName || m.actorId || "用户"}
                      </span>
                      <span className="ml-auto">
                        <Time value={m.createdAt} />
                      </span>
                    </div>
                    <p className="prose-body">
                      {m.deleted ? "该消息已撤回" : m.content}
                    </p>
                    <div className="mt-3 text-xs">
                      <SafeExternalLink href={m.sourceUrl}>
                        查看原消息
                      </SafeExternalLink>
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <div className="px-5 py-6 text-xs text-muted-foreground">
                暂无关联消息
              </div>
            )}
          </Panel>
          <Panel
            title="回复与草稿"
            action={
              <span className="inline-flex items-center gap-1 text-[11px] text-muted-foreground">
                <Bot className="size-3.5" />
                {item.replyMode === "manual" ? "人工处理中" : "跟随渠道设置"}
              </span>
            }
          >
            <div className="divide-y divide-border">
              {item.replies?.map((r) => (
                <div key={r.id} className="p-5">
                  <div className="mb-3 flex items-center justify-between">
                    <Status value={r.status} />
                    <Time value={r.createdAt} />
                  </div>
                  <p className="prose-body">{r.body || "暂无内容"}</p>
                  {r.evidenceIds?.length || r.imageEvidence?.length ? (
                    <div className="mt-3 flex flex-wrap gap-2">
                      {r.evidenceIds?.map((e) => (
                        <Link
                          className="rounded-md bg-muted px-2 py-1 font-mono text-[10px] text-primary"
                          key={e}
                          href={`/knowledge/${e}`}
                        >
                          引用知识 {shortId(e)}
                        </Link>
                      ))}
                      {r.imageEvidence?.map((image, index) => {
                        const data = objectData(image);
                        return (
                          <span className="rounded-md bg-muted px-2 py-1 font-mono text-[10px] text-muted-foreground" key={String(data.id ?? index)}>
                            引用图片 {String(data.name ?? data.id ?? index + 1)}
                          </span>
                        );
                      })}
                    </div>
                  ) : null}
                  <div className="mt-3 flex gap-2">
                    {r.body &&
                      r.status === "sent" &&
                      hasRole(me, "knowledge", "developer") && (
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() =>
                            setKnowledgeDraft({
                              title: item.title,
                              body: `## 问题\n\n${item.description || item.title}\n\n## 解答\n\n${r.body}`,
                              module: item.module,
                              visibility: item.channelId
                                ? "channel"
                                : "internal",
                              channelIds: item.channelId
                                ? [item.channelId]
                                : [],
                              sourceUrl: `${window.location.origin}/cases/${encodeURIComponent(id)}`,
                              sourceRevision: `reply:${r.id}:v${r.version ?? 1}`,
                            })
                          }
                        >
                          <BookOpen className="size-3" />
                          保存为知识草稿
                        </Button>
                      )}
                    {r.status === "draft" && canWrite && (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={!!busy}
                        onClick={async () => {
                          setBusy(r.id ?? "submit");
                          try {
                            await api.submitReply(r.id ?? "", {
                              expectedVersion: r.version ?? 0,
                            });
                            toast.success("回复已提交审批");
                            await mutate();
                          } catch (e) {
                            reportError(e);
                          } finally {
                            setBusy("");
                          }
                        }}
                      >
                        <Send className="size-3" />
                        提交审批
                      </Button>
                    )}
                    {r.approvalId && (
                      <Button variant="ghost" size="sm" asChild>
                        <Link href={`/approvals/${r.approvalId}`}>
                          查看审批
                          <ArrowRight className="size-3" />
                        </Link>
                      </Button>
                    )}
                  </div>
                </div>
              ))}
            </div>
            {canWrite && (
              <div className="border-t border-border p-5">
                <Textarea
                  aria-label="回复草稿"
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  placeholder="回复内容（请勿填写密码或 Token）"
                  className="min-h-32"
                />
                <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={!!busy}
                    onClick={() => void saveReply(true)}
                  >
                    {busy === "generate" ? (
                      <Loader2 className="size-3.5 animate-spin" />
                    ) : (
                      <Sparkles className="size-3.5" />
                    )}
                    生成回复草稿
                  </Button>
                  <Button
                    size="sm"
                    disabled={!!busy || !body.trim()}
                    onClick={() => void saveReply(false)}
                  >
                    {busy === "reply" && (
                      <Loader2 className="size-3.5 animate-spin" />
                    )}
                    保存草稿
                  </Button>
                </div>
              </div>
            )}
          </Panel>
          {canWrite && (
            <CaseFeedback
              caseId={id}
              version={item.version ?? 0}
              onSaved={(next) => next ? mutate(next, { revalidate: true }) : mutate()}
            />
          )}
          <Panel title="操作记录">
            {item.events?.length ? (
              <div className="px-5 py-3">
                {item.events.map((e, i) => {
                  const category = caseEventCategory(e.type, e.actorId);
                  return (
                    <div className="relative flex gap-3 py-3" key={e.id ?? i}>
                      <span className={cn("mt-2 size-2 shrink-0 rounded-full", activityDotClass[category.tone])} />
                      <div className="min-w-0 flex-1">
                        <div className="mb-1 flex flex-wrap items-center gap-2">
                          <ActivityMarker label={category.label} tone={category.tone} />
                          {e.type && <span className="text-[11px] text-muted-foreground">{activityAction(e.type, "cases")}</span>}
                        </div>
                        <p className="text-sm leading-6">{activityAction(e.body || e.type, "cases")}</p>
                        <Time value={e.createdAt} />
                        {canWrite && e === lastClassification && e.type === "needs_human" && (
                          <div className="mt-2 flex flex-wrap items-center gap-2">
                            <Button
                              variant="outline"
                              size="sm"
                              disabled={!!busy || item.replyMode === "manual" || item.status === "closed" || item.status === "resolved"}
                              onClick={() => void action("retry_classification")}
                            >
                              {busy === "retry_classification" ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
                              重新分类
                            </Button>
                            {(item.replyMode === "manual" || item.status === "closed" || item.status === "resolved") && <span className="text-xs text-muted-foreground">
                              {item.replyMode === "manual" ? "自动回复已暂停" : "问题已关闭或解决"}
                            </span>}
                          </div>
                        )}
                      </div>
                    </div>
                  );
                })}
              </div>
            ) : (
              <Empty
                title="暂无操作记录"
              />
            )}
          </Panel>
        </div>
        <aside className="space-y-5">
          <Panel title="处理信息">
            <div className="px-5 py-3">
              <Definition
                label="负责人"
                value={
                  people?.items?.find((p) => p.id === item.assigneeId)?.name ||
                  item.assigneeId ||
                  "未分配"
                }
              />
              <Definition label="支持范围" value={label(item.supportRelevance)} />
              <Definition label="模块" value={item.module || "未分类"} />
              <Definition label="类型" value={label(item.category || "unknown")} />
              <Definition label="来源" value={caseSourceLabel(item.source as string)} />
              {canWrite && (
                <div className="space-y-4 border-t border-border py-4">
                  <Field label="分派负责人">
                    <SearchSelect
                      value={item.assigneeId ?? ""}
                      placeholder="选择处理人"
                      options={(people?.items ?? [])
                        .filter((p) => p.active !== false)
                        .map((p) => ({
                          value: p.id ?? "",
                          label: p.name || p.email || p.id || "",
                        }))}
                      onChange={async (v) => {
                        try {
                          await api.updateCase(id, {
                            title: item.title ?? "",
                            assigneeId: v,
                            expectedVersion: item.version ?? 0,
                          });
                          toast.success("负责人已更新");
                          await mutate();
                        } catch (e) {
                          reportError(e);
                        }
                      }}
                    />
                  </Field>
                  <Field label="优先级">
                    <Select
                      value={item.priority || "P2"}
                      onChange={async (e) => {
                        try {
                          await api.updateCase(id, {
                            title: item.title ?? "",
                            priority: e.target.value,
                            expectedVersion: item.version ?? 0,
                          });
                          await mutate();
                        } catch (err) {
                          reportError(err);
                        }
                      }}
                    >
                      <option>P0</option>
                      <option>P1</option>
                      <option>P2</option>
                      <option>P3</option>
                    </Select>
                  </Field>
                </div>
              )}
            </div>
          </Panel>
          {item.developmentTaskId && (
            <Panel title="开发任务">
              <Link
                href={`/development/${item.developmentTaskId}`}
                className="flex items-center gap-2 p-5 text-sm text-primary"
              >
                <GitBranch className="size-4" />
                查看开发任务
                <ArrowRight className="ml-auto size-4" />
              </Link>
            </Panel>
          )}
          {canWrite && (
            <Panel title="处理操作">
              <div className="space-y-2 p-3">
                {!["resolved", "closed"].includes(item.status ?? "") && (
                  <Button
                    variant="ghost"
                    className="w-full justify-start"
                    disabled={!!busy}
                    onClick={() => void action("resolve")}
                  >
                    <Check className="size-4" />
                    标记已解决
                  </Button>
                )}
                <Button
                  variant="ghost"
                  className="w-full justify-start"
                  disabled={!!busy}
                  onClick={() =>
                    void action(
                      item.replyMode === "manual" ? "resume" : "takeover",
                    )
                  }
                >
                  {item.replyMode === "manual" ? (
                    <Play className="size-4" />
                  ) : (
                    <Pause className="size-4" />
                  )}
                  {item.replyMode === "manual"
                    ? "恢复自动回复"
                    : "接管并暂停自动回复"}
                </Button>
                <Button
                  variant="ghost"
                  className="w-full justify-start"
                  disabled={!!busy}
                  onClick={() =>
                    void action(
                      ["closed", "resolved"].includes(item.status ?? "")
                        ? "reopen"
                        : "close",
                    )
                  }
                >
                  <Check className="size-4" />
                  {["closed", "resolved"].includes(item.status ?? "")
                    ? "重新打开问题"
                    : "关闭问题"}
                </Button>
                <Button
                  variant="ghost"
                  className="w-full justify-start"
                  onClick={() => setRelationOpen(true)}
                >
                  <GitBranch className="size-4" />
                  关联重复问题
                </Button>
              </div>
            </Panel>
          )}
        </aside>
      </div>
      <Dialog
        open={!!knowledgeDraft}
        onOpenChange={(open) => {
          if (!open) setKnowledgeDraft(null);
        }}
        title="保存为知识草稿"
      >
        {knowledgeDraft && (
          <KnowledgeEditor
            value={knowledgeDraft}
            onCancel={() => setKnowledgeDraft(null)}
            onSave={async (value) => {
              const created = await api.createKnowledge({
                ...value,
                sourceRevision: knowledgeDraft.sourceRevision,
              });
              toast.success("草稿已保存");
              setKnowledgeDraft(null);
              if (created?.id) router.push(`/knowledge/${created.id}`);
            }}
          />
        )}
      </Dialog>
      <Dialog
        open={editOpen}
        onOpenChange={setEditOpen}
        title="编辑问题"
        description="修改内容或回复渠道后，已有审批可能失效。"
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            const data = Object.fromEntries(new FormData(e.currentTarget));
            let environment: Record<string, unknown>;
            try {
              environment = JSON.parse(String(data.environment || "{}"));
              if (
                !environment ||
                Array.isArray(environment) ||
                typeof environment !== "object"
              )
                throw new Error();
            } catch {
              toast.error("环境信息必须是 JSON 对象");
              return;
            }
            setBusy("edit");
            try {
              await api.updateCase(id, {
                title: String(data.title),
                description: String(data.description),
                module: String(data.module),
                category: String(data.category),
                supportRelevance: String(data.supportRelevance),
                channelId: String(data.channelId),
                environment: { additionalData: environment },
                expectedVersion: item.version ?? 0,
              });
              setEditOpen(false);
              toast.success("问题已更新");
              await mutate();
            } catch (err) {
              reportError(err);
            } finally {
              setBusy("");
            }
          }}
        >
          <Field label="标题">
            <Input
              name="title"
              defaultValue={item.title ?? ""}
              required
              minLength={3}
            />
          </Field>
          <Field label="描述">
            <Textarea
              name="description"
              defaultValue={item.description ?? ""}
              className="min-h-32"
              required
            />
          </Field>
          <div className="grid grid-cols-2 gap-4">
            <Field label="模块">
              <Input name="module" defaultValue={item.module ?? ""} />
            </Field>
            <Field label="支持范围">
              <Select
                name="supportRelevance"
                defaultValue={item.supportRelevance ?? "uncertain"}
              >
                <option value="in_scope">范围内</option>
                <option value="out_of_scope">范围外</option>
                <option value="uncertain">待判断</option>
              </Select>
            </Field>
          </div>
          <Field label="类型">
            <Select name="category" defaultValue={item.category ?? "unknown"}>
              {["unknown", "question", "bug", "feedback", "feature", "security"].map(
                (v) => (
                  <option key={v} value={v}>
                    {label(v)}
                  </option>
                ),
              )}
            </Select>
          </Field>
          <Field
            label="回复渠道"
          >
            <Select name="channelId" defaultValue={item.channelId ?? ""}>
              <option value="">不指定渠道</option>
              {channels?.items?.map((c) => (
                <option key={c.id} value={c.id ?? ""}>
                  {c.name || c.externalId}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="环境信息（JSON）">
            <Textarea
              name="environment"
              defaultValue={JSON.stringify(
                objectData(item.environment),
                null,
                2,
              )}
              className="font-mono text-xs"
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setEditOpen(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={!!busy}>
              保存修改
            </Button>
          </div>
        </form>
      </Dialog>
      <Dialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title="创建开发任务"
      >
        <form className="space-y-4" onSubmit={confirm}>
          <Field label="目标仓库">
            <SearchSelect
              value={repository}
              onChange={setRepository}
              options={(repos?.items ?? [])
                .filter((r) => r.enabled !== false)
                .map((r) => ({
                  value: r.id ?? "",
                  label: `${r.owner}/${r.repo}`,
                }))}
              placeholder="选择仓库"
            />
          </Field>
          <Field label="修改目标">
            <Textarea
              name="goal"
              defaultValue={item.description || item.title || ""}
              required
            />
          </Field>
          <Field label="验收条件">
            <Textarea
              name="acceptanceCriteria"
              placeholder="填写预期结果和需要通过的测试"
              required
            />
          </Field>
          <Field label="后续补充"><Select name="followupMode" defaultValue="manual"><option value="manual">由我确认后应用</option><option value="within_scope">自动应用范围内补充</option></Select></Field>
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setConfirmOpen(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={!!busy}>
              {busy === "confirm" && (
                <Loader2 className="size-4 animate-spin" />
              )}
              创建任务
            </Button>
          </div>
        </form>
      </Dialog>
      <Dialog
        open={relationOpen}
        onOpenChange={setRelationOpen}
        title="关联重复问题"
      >
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            const f = new FormData(e.currentTarget);
            setBusy("relation");
            try {
              await api.relateCase(id, {
                targetId: String(f.get("targetId")),
                reason: String(f.get("reason")),
                expectedVersion: item.version ?? 0,
              });
              toast.success("关联已保存");
              setRelationOpen(false);
              await mutate();
            } catch (err) {
              reportError(err);
            } finally {
              setBusy("");
            }
          }}
        >
          <Field label="要关联的问题 ID">
            <Input name="targetId" required placeholder="粘贴完整的问题 ID" />
          </Field>
          <Field label="关联原因">
            <Textarea
              name="reason"
              required
              placeholder="说明重复之处"
            />
          </Field>
          <Button type="submit" disabled={!!busy}>
            保存关联
          </Button>
        </form>
      </Dialog>
    </>
  );
}
