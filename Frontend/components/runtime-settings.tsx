"use client";

import { useState } from "react";
import Link from "next/link";
import useSWR from "swr";
import { Switch } from "radix-ui";
import { AlertTriangle, Check, Loader2, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { api, useMe } from "@/lib/api/client";
import type { ExecutionSettings, ModelSettings, RunnerCapabilities } from "@/lib/api/generated/models";
import { hasRole } from "@/lib/access";
import {
  changeResourceLimits,
  executionFields,
  executionFormValues,
  executionSettingsPayload,
  executionRuntimeIssue,
  modelFormValues,
  modelSettingsPayload,
  settingsValidationMessage,
  runtimeHostKnown,
  cgroupRuntimeLabel,
  type ExecutionFormValues,
  type ModelFormValues,
} from "@/lib/settings-runtime";
import { cn } from "@/lib/utils";
import { ErrorState, Loading, Panel, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/input";

function Toggle({ label, hint, checked, onChange }: {
  label: string; hint?: string; checked: boolean; onChange: (value: boolean) => void;
}) {
  return <div className="flex items-center justify-between gap-5 rounded-lg border border-border p-4">
    <div><p className="text-sm font-medium">{label}</p>{hint && <p className="mt-1 text-xs leading-5 text-muted-foreground">{hint}</p>}</div>
    <Switch.Root aria-label={label} checked={checked} onCheckedChange={onChange} className="relative h-6 w-10 shrink-0 rounded-full bg-stone-200 transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 focus-visible:ring-offset-2 disabled:opacity-50 data-[state=checked]:bg-primary">
      <Switch.Thumb className="block size-4 translate-x-1 rounded-full bg-white shadow transition data-[state=checked]:translate-x-5" />
    </Switch.Root>
  </div>;
}

function ReadOnlyNote({ admin }: { admin: boolean }) {
  return !admin && <p className="border-b border-border bg-muted/50 px-5 py-3 text-xs text-muted-foreground">当前为只读模式，仅管理员可修改此配置。</p>;
}

function RefreshNotice({ retry }: { retry: () => void }) {
  return <div role="status" className="flex flex-wrap items-center justify-between gap-3 border-b border-amber-200 bg-amber-50 px-5 py-3 text-xs text-amber-900"><p>刷新失败，当前仍显示上次加载的配置，未保存的修改已保留。</p><Button type="button" variant="outline" size="sm" onClick={retry}>重试</Button></div>;
}

function VersionNotice({ stale, reload }: { stale: boolean; reload: () => void }) {
  return stale && <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-200 bg-amber-50 p-4 text-xs text-amber-900">
    <p>配置已被他人更新，请先载入最新版本再继续编辑（会覆盖当前未保存的内容）。</p>
    <Button type="button" variant="outline" size="sm" onClick={reload}><RefreshCw className="size-3.5" />载入最新版</Button>
  </div>;
}

function SaveFooter({ admin, busy, stale }: {
  admin: boolean; busy: boolean; stale: boolean;
}) {
  return admin && <div className="flex flex-wrap items-center justify-between gap-4 border-t border-border pt-5">
    <p className="text-xs text-muted-foreground">保存后立即生效；尚未执行的旧审批会失效，需要重新审批。</p>
    <Button type="submit" disabled={busy || stale}>{busy ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}{busy ? "保存中…" : "保存"}</Button>
  </div>;
}

export function ModelSettingsPanel() {
  const { data: me } = useMe();
  const { data, error, isLoading, mutate } = useSWR("model-settings", api.modelSettings);
  return <Panel title="模型接口">
    {data ? <>{error && <RefreshNotice retry={() => void mutate()} />}<ModelForm settings={data} admin={hasRole(me, "admin")} saved={async (next) => { await mutate(next, { revalidate: false }); }} /></> : isLoading ? <Loading /> : <ErrorState error={error ?? new Error("未能读取模型配置")} retry={() => void mutate()} />}
  </Panel>;
}

function ModelForm({ settings, admin, saved }: { settings: ModelSettings; admin: boolean; saved: (next: ModelSettings) => Promise<void> }) {
  const [values, setValues] = useState(() => modelFormValues(settings));
  const [version, setVersion] = useState(settings.version ?? 0);
  const [busy, setBusy] = useState(false);
  const stale = version !== (settings.version ?? 0);
  function change<K extends keyof ModelFormValues>(key: K, value: ModelFormValues[K]) { setValues((current) => ({ ...current, [key]: value })); }
  function reload() { setValues(modelFormValues(settings)); setVersion(settings.version ?? 0); }
  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (!admin || stale || busy) return;
    let body;
    try { body = modelSettingsPayload(values, version, settings); } catch (error) { toast.error(settingsValidationMessage(error)); return; }
    setBusy(true);
    try {
      const next = await api.saveModelSettings(body);
      if (!next) throw new Error("服务未返回保存结果，请刷新确认");
      setValues(modelFormValues(next));
      setVersion(next.version ?? 0);
      await saved(next);
      toast.success("模型设置已保存");
    } catch (error) { reportError(error); } finally { setBusy(false); }
  }
  return <>
    <ReadOnlyNote admin={admin} />
    <form onSubmit={submit} className="space-y-6 p-5 sm:p-6">
      <VersionNotice stale={stale} reload={reload} />
      <fieldset disabled={!admin || busy} className="space-y-6">
        <legend className="sr-only">模型配置</legend>
        <Toggle label="启用模型" hint={values.enabled ? "开启后，问题内容、知识片段与任务代码将发送至所选模型服务。" : undefined} checked={values.enabled} onChange={(value) => change("enabled", value)} />
        <div className="grid gap-5 md:grid-cols-2">
          <Field label="API 地址" hint="填写接口根路径即可，无需包含 /chat/completions">
            <Input type="url" maxLength={2048} value={values.baseUrl} onChange={(event) => change("baseUrl", event.target.value)} placeholder="https://api.example.com/v1" required={values.enabled} autoComplete="off" />
          </Field>
          <Field label="模型名称">
            <Input maxLength={200} value={values.modelName} onChange={(event) => change("modelName", event.target.value)} placeholder="输入模型名称" required={values.enabled} autoComplete="off" />
          </Field>
          <Field label="API 密钥">
            <Input type="password" maxLength={8192} value={values.apiKey} onChange={(event) => change("apiKey", event.target.value)} disabled={values.clearApiKey} placeholder={settings.apiKeyConfigured ? "留空保持现有密钥" : "输入 API 密钥"} autoComplete="new-password" />
          </Field>
          <Field label="每日 Token 上限">
            <Input type="number" min={1} max={1000000000} step={1} required value={values.maxDailyTokens} onChange={(event) => change("maxDailyTokens", event.target.value)} />
          </Field>
        </div>
        {settings.apiKeyConfigured && <label className="flex items-center gap-2 text-xs text-muted-foreground"><input type="checkbox" className="size-4 accent-primary" checked={values.clearApiKey} onChange={(event) => setValues((current) => ({ ...current, clearApiKey: event.target.checked, apiKey: "" }))} />清除已有密钥</label>}
        <details className="rounded-lg border border-border px-4 py-3">
          <summary className="cursor-pointer text-sm font-medium">接口兼容选项</summary>
          <div className="mt-5 space-y-5">
            <div className="grid gap-5 md:grid-cols-2">
              <Field label="JSON 输出格式">
                <Select value={values.jsonMode} onChange={(event) => change("jsonMode", event.target.value as ModelFormValues["jsonMode"])}><option value="json_schema">JSON Schema（结构化输出）</option><option value="json_object">JSON Object</option></Select>
              </Field>
              <Field label="Token 上限参数">
                <Select value={values.maxTokensField} onChange={(event) => change("maxTokensField", event.target.value as ModelFormValues["maxTokensField"])}><option value="max_tokens">max_tokens</option><option value="max_completion_tokens">max_completion_tokens</option></Select>
              </Field>
            </div>
            <Toggle label="允许 HTTP 地址" hint={values.allowHttp ? "仅建议用于本地或内网调试，密钥与请求内容将以明文传输。" : undefined} checked={values.allowHttp} onChange={(value) => change("allowHttp", value)} />
          </div>
        </details>
      </fieldset>
      <SaveFooter admin={admin} busy={busy} stale={stale} />
    </form>
  </>;
}

export function ExecutionSettingsPanel() {
  const { data: me } = useMe();
  const { data, error, isLoading, mutate } = useSWR("execution-settings", api.executionSettings, { refreshInterval: 10000 });
  return <div className="space-y-5">
    {data && error && <RefreshNotice retry={() => void mutate()} />}
    {data && <ExecutionRuntime settings={data} refresh={() => void mutate()} />}
    <Panel title="任务执行">
      {data ? <ExecutionForm settings={data} admin={hasRole(me, "admin")} saved={async (next) => { await mutate(next, { revalidate: false }); }} /> : isLoading ? <Loading /> : <ErrorState error={error ?? new Error("未能读取执行配置")} retry={() => void mutate()} />}
    </Panel>
  </div>;
}

function ExecutionRuntime({ settings, refresh }: { settings: ExecutionSettings; refresh: () => void }) {
  const machines = settings.runtime?.machines;
  return <Panel title="运行机器状态" action={<Button type="button" variant="ghost" size="sm" onClick={refresh}><RefreshCw className="size-3.5" />刷新</Button>}>
    <div className="divide-y divide-border">
      {machines?.length ? machines.map((runtime) => <RuntimeMachineStatus key={runtime.id ?? runtime.name} runtime={runtime} />) : <RuntimeMachineStatus runtime={settings.runtime} />}
    </div>
  </Panel>;
}

function RuntimeMachineStatus({ runtime }: { runtime?: RunnerCapabilities | null }) {
  const connected = runtime?.reachable === true;
  const ready = connected && runtime?.ready === true;
  const hostKnown = runtimeHostKnown(runtime);
  const issue = executionRuntimeIssue(runtime);
  const status = !connected ? "未连接" : ready ? "已就绪" : runtime?.ready === false ? "未就绪" : "检查中";
  const capability = (value: boolean | null | undefined, yes: string, no: string) => value === true ? yes : value === false ? no : "待确认";
  return <div className="space-y-4 p-5">
      <div className="flex flex-wrap items-center gap-3">{runtime?.name && <h3 className="text-sm font-medium">{runtime.name}</h3>}<span className={cn("rounded-md px-2.5 py-1 text-xs font-medium", ready ? "bg-emerald-50 text-emerald-700" : "bg-amber-50 text-amber-800")}>{status}</span></div>
      {connected ? <dl className="grid gap-4 text-sm sm:grid-cols-2 xl:grid-cols-3">
        {[
          ["Rootless 容器环境", capability(hostKnown ? runtime?.rootless : undefined, "已就绪", "未通过")],
          ["运行盘与目录", capability(hostKnown ? runtime?.filesystemReady : undefined, "已就绪", "需检查")],
          ["CPU / 内存 / 进程限制", capability(hostKnown ? runtime?.supportsResourceLimits : undefined, "支持", "不支持")],
          ["执行配置", capability(runtime?.settingsConfigured, "已同步", "未同步")],
          ["模型接口", capability(runtime?.modelConfigured, "已启用", "未就绪")],
          ["正在运行", `${runtime?.activeRuns ?? "—"} 个任务`],
        ].map(([key, value]) => <div key={key}><dt className="text-muted-foreground">{key}</dt><dd className="mt-1.5 font-medium">{value}</dd></div>)}
      </dl> : <Link href="/settings/runtimeMachines" className="text-sm text-primary hover:underline">管理运行机器</Link>}
      {connected && <p className="text-sm leading-6 text-muted-foreground">资源模式：{runtime?.resourceLimits === "disabled" ? "未启用 CPU / 内存 / 进程限制" : runtime?.resourceLimits === "required" ? "启用资源限制" : "待确认"}；并发上限 {runtime?.maxConcurrent ?? "—"} 个任务。{cgroupRuntimeLabel(runtime)}。</p>}
      {connected && <dl className="grid gap-4 border-t border-border pt-4 text-sm sm:grid-cols-3">
        {[
          ["网络", runtime?.networkMode === "bridge" ? "可联网" : runtime?.networkMode === "none" ? "离线" : "待确认"],
          ["根文件系统", capability(runtime?.readOnlyRootfs, "只读", "可写")],
          ["测试目录", capability(runtime?.testWorkspaceWritable, "临时副本可写", "只读")],
        ].map(([key, value]) => <div key={key}><dt className="text-muted-foreground">{key}</dt><dd className="mt-1.5 font-medium">{value}</dd></div>)}
      </dl>}
      {issue && <p role="status" className="rounded-lg bg-amber-50 px-3 py-2 text-xs leading-6 text-amber-900">{issue.message}{issue.modelLink && <Link href="/settings/model" className="ml-2 font-medium underline underline-offset-4">去配置模型</Link>}</p>}
    </div>;
}

function ExecutionForm({ settings, admin, saved }: { settings: ExecutionSettings; admin: boolean; saved: (next: ExecutionSettings) => Promise<void> }) {
  const [values, setValues] = useState(() => executionFormValues(settings));
  const [version, setVersion] = useState(settings.version ?? 0);
  const [busy, setBusy] = useState(false);
  const stale = version !== (settings.version ?? 0);
  const quotasDisabled = values.resourceLimits === "disabled";
  const quotasUnavailable = runtimeHostKnown(settings.runtime) && settings.runtime?.supportsResourceLimits === false;
  function reload() { setValues(executionFormValues(settings)); setVersion(settings.version ?? 0); }
  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (!admin || stale || busy) return;
    let body;
    try { body = executionSettingsPayload(values, version); } catch (error) { toast.error(settingsValidationMessage(error)); return; }
    setBusy(true);
    try {
      const next = await api.saveExecutionSettings(body);
      if (!next) throw new Error("服务未返回保存结果，请刷新确认");
      setValues(executionFormValues(next));
      setVersion(next.version ?? 0);
      await saved(next);
      toast.success("任务执行设置已保存");
    } catch (error) { reportError(error); } finally { setBusy(false); }
  }
  return <>
    <ReadOnlyNote admin={admin} />
    <form onSubmit={submit} className="space-y-6 p-5 sm:p-6">
      <VersionNotice stale={stale} reload={reload} />
      <fieldset disabled={!admin || busy} className="space-y-6">
        <legend className="sr-only">执行配置</legend>
        <Link href="/settings/policies" className="text-sm text-primary hover:underline">配置审批策略 →</Link>
        <section className="space-y-4 border-t border-border pt-5">
          <h3 className="text-sm font-medium">运行环境</h3>
          <Field label="任务网络" hint={values.networkMode === "bridge" ? "开启后任务容器可访问宿主机网络可达的地址，不做域名限制。" : undefined}>
            <Select value={values.networkMode} onChange={(event) => setValues((current) => ({ ...current, networkMode: event.target.value as ExecutionFormValues["networkMode"] }))}>
              <option value="none">离线（默认）</option>
              <option value="bridge">允许联网</option>
            </Select>
          </Field>
          <Toggle label="根文件系统只读" hint={!values.readOnlyRootfs ? "关闭后，容器内当前用户有权限的目录均可写入（任务结束即销毁）。" : undefined} checked={values.readOnlyRootfs} onChange={(value) => setValues((current) => ({ ...current, readOnlyRootfs: value }))} />
          <Toggle label="允许测试写入临时目录" hint={values.testWorkspaceWritable ? "在独立临时副本中运行测试以便安装依赖；新增文件不会进入 PR，改动已有源码或锁文件将判定测试失败。" : undefined} checked={values.testWorkspaceWritable} onChange={(value) => setValues((current) => ({ ...current, testWorkspaceWritable: value }))} />
        </section>
        <Field label="CPU、内存和进程限制">
          <Select value={values.resourceLimits} onChange={(event) => setValues((current) => changeResourceLimits(current, event.target.value as "required" | "disabled"))}>
            <option value="required" disabled={quotasUnavailable}>开启限制（推荐）{quotasUnavailable ? " — 当前机器不支持" : ""}</option>
            <option value="disabled">关闭限制（仅内部开发环境）</option>
          </Select>
        </Field>
        {quotasUnavailable && !quotasDisabled && <p role="status" className="rounded-lg bg-amber-50 p-4 text-xs leading-6 text-amber-900">当前机器不支持资源限制，任务无法启动。请检查宿主机 cgroup 配置；仅内部开发环境可关闭限制。</p>}
        {quotasDisabled && <div role="status" className="flex gap-3 rounded-lg border border-amber-200 bg-amber-50 p-4 text-xs leading-6 text-amber-900"><AlertTriangle className="mt-1 size-4 shrink-0" aria-hidden="true" /><p>关闭后不再限制 CPU、内存与进程数，任务可能耗尽宿主机资源，仅限内部开发环境使用；同时并发数固定为 1。</p></div>}
        {["运行限制", "用量上限（0 为不限）", "重试策略"].map((group) => <section key={group} className="space-y-4">
          <h3 className="border-t border-border pt-5 text-sm font-medium">{group}</h3>
          <div className="grid gap-5 sm:grid-cols-2 xl:grid-cols-3">{executionFields.filter((field) => field.group === group).map((field) => {
            const inactive = quotasDisabled && (("resource" in field && field.resource) || field.key === "maxConcurrent");
            return <Field key={field.key} label={field.label} hint={field.key === "externalRetryAttempts" && Number(values.externalRetryAttempts) > 0 ? "开启自动重试后，网络超时等不确定场景可能导致重复发送。" : "hint" in field ? field.hint : undefined}>
              <Input type="number" min={field.min} max={"max" in field ? field.max : undefined} step={1} required disabled={inactive} value={values[field.key]} onChange={(event) => setValues((current) => ({ ...current, [field.key]: event.target.value }))} />
            </Field>;
          })}</div>
        </section>)}
      </fieldset>
      <SaveFooter admin={admin} busy={busy} stale={stale} />
    </form>
  </>;
}
