"use client";
import { useState } from "react";
import useSWR from "swr";
import { runtimeRead, runtimeWrite } from "@/lib/api/client";
import { Panel, ErrorState, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Textarea, Field, Select, Input } from "@/components/ui/input";
import { toast } from "sonner";

type TaskInput = { id: string; seq: number; body: string; disposition: string; reason_code: string; actor_ref?: { name?: string; subject_id?: string; principal_id?: string }; source_version?: number; applied_revision?: number; receipts?: { phase: string; part_id: string }[] };
type Runtime = { version: number; next_cursor?: string; hold_reason?: string; inputs: TaskInput[]; threads: { thread_id: string; relation: string; routing_active: boolean; source: { external_thread_id: string } }[]; authorization?: { issuer: string; status: string; scope: { followup_mode: string } } };
const names: Record<string, string> = { pending: "待判断", review: "待确认", apply: "待应用", context: "参考信息", unrelated: "与任务无关", withdrawn: "已撤回", superseded: "已有更新" };
export function TaskInputs({ id, goal, acceptance, writable, refresh }: { id: string; goal: string; acceptance: string; writable: boolean; refresh: () => Promise<unknown> }) {
  const path = `/api/v1/development-tasks/${id}`;
  const [cursor, setCursor] = useState("");
  const { data, error, mutate } = useSWR(["task-runtime", id, cursor], () => runtimeRead<Runtime>(`${path}/runtime?cursor=${cursor}`), { refreshInterval: 5000 });
  const [body, setBody] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [followup, setFollowup] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [sourceQuery, setSourceQuery] = useState("");
  const [sourceCursor, setSourceCursor] = useState("");
  const [thread, setThread] = useState("");
  const [relation, setRelation] = useState("context");
  const { data: sources } = useSWR(writable ? ["source-threads", id, sourceQuery, sourceCursor] : null, () => runtimeRead<{ items: { id: string; title: string }[]; next_cursor?: string }>(`/api/v1/source-threads?q=${encodeURIComponent(sourceQuery)}&cursor=${sourceCursor}`));
  async function perform(action: () => Promise<unknown>) {
    setBusy(true);
    try { await action(); await mutate(); await refresh(); } catch (e) { reportError(e); } finally { setBusy(false); }
  }
  if (error) return <ErrorState error={error} retry={() => void mutate()} />;
  if (!data) return null;
  return <Panel title="补充信息">
    {data.authorization && <p className="mb-4 text-xs text-muted-foreground">{data.authorization.issuer === "policy_auto" ? "按审批策略授权" : "由开发者确认"} · {data.authorization.status === "active" ? "授权有效" : "需要重新确认"}</p>}
    <fieldset disabled={busy} className="space-y-3"><legend className="sr-only">任务补充信息</legend>
      {!data.inputs.length && <p className="text-sm text-muted-foreground">还没有补充信息</p>}
      {data.inputs.map(input => <div key={input.id} className="rounded-lg border border-border p-3">
        <div className="flex items-start gap-3">
          {writable && !["withdrawn", "superseded"].includes(input.disposition) && <input type="checkbox" className="mt-1 size-4 accent-primary" aria-label={`选择第 ${input.seq} 条补充信息`} checked={selected.includes(input.id)} onChange={e => setSelected(ids => e.target.checked ? [...ids, input.id] : ids.filter(x => x !== input.id))} />}
          <div className="min-w-0 flex-1">
            {(input.actor_ref?.name || input.actor_ref?.subject_id) && <p className="mb-2 text-xs text-muted-foreground">{input.actor_ref.name || input.actor_ref.subject_id}{input.source_version ? ` · 消息版本 ${input.source_version}` : ""}</p>}
            <p className="whitespace-pre-wrap break-words text-sm">{input.body || "消息已撤回"}</p>
            <p className="mt-2 text-xs text-muted-foreground">{input.applied_revision ? `已纳入版本 ${input.applied_revision}` : names[input.disposition] || input.disposition}{input.receipts?.some(r => r.phase === "included") ? " · 已有内容进入模型请求" : input.receipts?.some(r => r.phase === "delivered") ? " · 已送达执行器" : ""}</p>
            {input.reason_code && <details className="mt-2 text-xs text-muted-foreground"><summary>处理依据</summary><p className="mt-2">{input.reason_code}</p></details>}
          </div>
        </div>
        {writable && !input.applied_revision && ["pending", "review", "apply"].includes(input.disposition) && <div className="mt-3 flex gap-2">{[["context", "仅供参考"], ["unrelated", "与任务无关"]].map(([action, title]) => <Button key={action} size="sm" variant="outline" onClick={() => void perform(() => runtimeWrite(`${path}/inputs/${input.id}/actions`, "POST", { action, expected_version: data.version }))}>{title}</Button>)}</div>}
      </div>)}
      {(cursor || data.next_cursor) && <div className="flex gap-2"><Button variant="outline" onClick={() => setCursor("")} disabled={!cursor}>最新信息</Button><Button variant="outline" disabled={!data.next_cursor} onClick={() => setCursor(data.next_cursor ?? "")}>更早的信息</Button></div>}
      {writable && <>
        <details className="rounded-lg border p-3"><summary className="text-sm">关联会话</summary><div className="mt-3 space-y-3">
          {data.threads?.filter(t => t.routing_active).map(t => <div key={t.thread_id} className="flex items-center justify-between gap-3 text-sm"><span>{sources?.items.find(s => s.id === t.thread_id)?.title || t.source.external_thread_id} · {t.relation === "owner" ? "接收跟进" : "参考上下文"}</span><Button size="sm" variant="outline" onClick={() => void perform(() => runtimeWrite(`${path}/threads/${t.thread_id}`, "DELETE", { expected_version: data.version }))}>解除关联</Button></div>)}
          <Field label="查找会话"><Input value={sourceQuery} onChange={e => { setSourceQuery(e.target.value); setSourceCursor(""); }} /></Field>
          <Field label="选择会话"><Select value={thread} onChange={e => setThread(e.target.value)}><option value="">选择问题会话</option>{sources?.items.map(s => <option key={s.id} value={s.id}>{s.title}</option>)}</Select></Field>
          {sources?.next_cursor && <Button variant="ghost" onClick={() => setSourceCursor(sources.next_cursor ?? "")}>更多会话</Button>}
          <Field label="用途"><Select value={relation} onChange={e => setRelation(e.target.value)}><option value="context">仅供参考</option><option value="owner">接收任务跟进</option></Select></Field>
          <Button variant="outline" disabled={!thread} onClick={() => void perform(async () => { await runtimeWrite(`${path}/threads`, "POST", { thread_id: thread, relation, expected_version: data.version }); setThread(""); })}>关联会话</Button>
        </div></details>
        {data.authorization?.status === "active" && <Button variant="outline" onClick={() => void perform(() => runtimeWrite(`${path}/actions`, "POST", { action: "revoke", expected_version: data.version }))}>撤销任务授权</Button>}
        <Field label="补充内容"><Textarea value={body} onChange={e => setBody(e.target.value)} rows={3} maxLength={64000} /></Field>
        <Button disabled={!body.trim()} onClick={() => void perform(async () => { await runtimeWrite(`${path}/inputs`, "POST", { body, expected_version: data.version }); setBody(""); toast.success("已添加"); })}>添加补充</Button>
        {selected.length > 0 && <div className="space-y-3 rounded-lg bg-muted p-3">
          <Field label="后续补充"><Select value={followup ?? data.authorization?.scope.followup_mode ?? "manual"} onChange={e => setFollowup(e.target.value)}><option value="manual">由我确认后应用</option><option value="within_scope">自动应用范围内补充</option></Select></Field>
          <div className="flex flex-wrap gap-2"><Button onClick={() => void perform(async () => { await runtimeWrite(`${path}/revisions`, "POST", { goal, acceptance_criteria: acceptance, input_ids: selected, followup_mode: followup ?? data.authorization?.scope.followup_mode ?? "manual", expected_version: data.version }); setSelected([]); toast.success("已创建新版本"); })}>应用所选信息</Button><Button variant="outline" onClick={() => void perform(async () => { await runtimeWrite(`${path}/revisions`, "POST", { goal, acceptance_criteria: acceptance, excluded_input_ids: selected, followup_mode: followup ?? data.authorization?.scope.followup_mode ?? "manual", expected_version: data.version }); setSelected([]); toast.success("已改为参考信息"); })}>仅作参考</Button><Button variant="outline" onClick={() => void perform(() => runtimeWrite(`${path}/actions`, "POST", { action: "pause", expected_version: data.version }))}>暂停当前执行</Button></div>
        </div>}
      </>}
    </fieldset>
  </Panel>;
}
