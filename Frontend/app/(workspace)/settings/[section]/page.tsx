"use client";
import { Fragment, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import useSWR from "swr";
import { Switch } from "radix-ui";
import {
  Check,
  BookOpen,
  Copy,
  FilePenLine,
  Info,
  MessageSquare,
  Loader2,
  Plus,
  RefreshCw,
  Trash2,
  UsersRound,
} from "lucide-react";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  settingsSections,
  settingFields,
  settingPayload,
  changeSettingValue,
  platformNames,
  type SettingField,
} from "@/lib/settings";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Empty,
  Status,
  Time,
  reportError,
  label,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input, Textarea, Select, Field } from "@/components/ui/input";
import { SearchSelect } from "@/components/search-select";
import { cn, objectData } from "@/lib/utils";
import { ConnectionCapabilities } from "@/components/connection-capabilities";
import { ExecutionSettingsPanel, ModelSettingsPanel } from "@/components/runtime-settings";
import type { Connector } from "@/lib/api/generated/models";

type RecordValue = Record<string, unknown>;
const navigationGroups = [
  {
    label: "消息与反馈", icon: MessageSquare, links: [
      { id: "connections", label: "平台账号" },
      { id: "channels", label: "消息渠道" },
    ]
  },
  {
    label: "开发与知识", icon: BookOpen, links: [
      { id: "repositories", label: "代码仓库" },
      { id: "runtimeMachines", label: "运行机器" },
      { id: "sources", label: "知识来源" },
      { id: "model", label: "模型接口" },
      { id: "execution", label: "任务执行" },
    ]
  },
  {
    label: "团队与流程", icon: UsersRound, links: [
      { id: "team", label: "团队成员" },
      { id: "identities", label: "身份绑定" },
      { id: "routing", label: "值班安排" },
      { id: "policies", label: "审批策略" },
    ]
  },
];
export default function SettingsPage() {
  const { section } = useParams<{ section: string }>();
  return <SettingsSection key={section} section={section} />;
}
function SettingsSection({ section }: { section: string }) {
  const config = settingsSections[section];
  const { data: me } = useMe();
  const [record, setRecord] = useState<RecordValue | null>(null);
  const [deleteRecord, setDeleteRecord] = useState<RecordValue | null>(null);
  const [forceDelete, setForceDelete] = useState(false);
  const [values, setValues] = useState<RecordValue>({});
  const [busy, setBusy] = useState(false);
  const [running, setRunning] = useState("");
  const [deleting, setDeleting] = useState("");
  const [capabilityId, setCapabilityId] = useState<string | null>(null);
  const { data, error, isLoading, mutate } = useSWR<
    Awaited<ReturnType<typeof api.listSettings>>
  >(
    config ? ["settings", config.resource] : null,
    () => api.listSettings(config.resource, { limit: 100 }),
    { refreshInterval: section === "connections" ? 5000 : 0 },
  );
  const { data: people } = useSWR("principals", () =>
    api.principals({ limit: 100 }),
  );
  const { data: connectors } = useSWR("connectors-options", () =>
    api.connectors({ limit: 100 }),
  );
  const { data: channels } = useSWR("channels-options", () =>
    api.channels({ limit: 100 }),
  );
  const { data: repos } = useSWR("repositories", () =>
    api.repositories({ limit: 100 }),
  );
  const { data: runtimeMachines } = useSWR("runtime-machines", () =>
    api.runtimeMachines({ limit: 100 }),
  );
  const lookups: Record<string, Array<{ id?: string | null; name?: string | null; provider?: string | null }>> = {
    principals: people?.items ?? [],
    connectors: connectors?.items ?? [],
    channels: channels?.items ?? [],
    repositories: repos?.items ?? [],
    runtimeMachines: runtimeMachines?.items ?? [],
  };
  const fields = settingFields(section, values, connectors?.items ?? []).map(field => section === "policies" && values.kind === "create_task" && field.key === "allowedPaths" ? { ...field, hint: "", options: (repos?.items?.find(repo => repo.id === values.repositoryId)?.allowedPaths ?? []).map(path => ({value:path,label:path})) } : field);
  const providerName = platformNames[String(values.provider)] || "平台";
  function changeValue(key: string, value: unknown) {
    setValues((current) => changeSettingValue(section, current, key, value));
  }
  const admin =
    hasRole(me, "admin") || (section === "sources" && hasRole(me, "knowledge"));
  const capabilityRecord =
    section === "connections"
      ? (data?.items as Connector[] | undefined)?.find(
        (item) => item.id === capabilityId,
      )
      : undefined;
  function options(field: SettingField) {
    if (!field.lookup) return field.options ?? [];
    return lookups[field.lookup]
      .filter(item => field.key !== "notificationChannelId" || channels?.items?.some(channel => channel.id === item.id && channel.enabled && channel.visibility === "internal"))
      .filter(item => field.key !== "sponsorPrincipalId" || people?.items?.some(person => person.id === item.id && person.active && hasRole(person, "developer") && (hasRole(person,"admin") || ((person.channelIds?.includes("*") || person.channelIds?.includes(String(values.channelId))) && (person.repositoryIds?.includes("*") || person.repositoryIds?.includes(String(values.repositoryId)))))))
      .filter((item: { id?: string | null; name?: string | null; provider?: string | null }) => section !== "sources" || field.lookup !== "connectors" ||
        ("provider" in item && ["feishu", "lark"].includes(String(item.provider))))
      .map((item: { id?: string | null; name?: string | null; provider?: string | null }) => ({
        value: item.id ?? "",
        label: field.lookup === "connectors" && "provider" in item
          ? `${item.name || item.id} · ${platformNames[String(item.provider)] || item.provider}`
          : item.name || item.id || "",
      }));
  }
  function edit(r: RecordValue) {
    setRecord(r);
    const next: RecordValue = {};
    for (const f of config.fields) {
      next[f.key] =
        f.kind === "password"
          ? ""
          : f.kind === "json"
            ? JSON.stringify(
              objectData(
                r[f.key] as
                { additionalData?: Record<string, unknown> } | undefined,
              ),
              null,
              2,
            )
            : (r[f.key] ??
              f.default ??
              (f.kind === "boolean" ? false : f.kind === "lines" ? [] : ""));
    }
    if (section === "policies" && r.expiresAt) { const date = new Date(String(r.expiresAt)); if (Number.isFinite(date.getTime())) next.expiresAt = new Date(date.getTime() - date.getTimezoneOffset()*60000).toISOString().slice(0,16); }
    if (section === "policies" && !r.id) next.senderTypes = ["human"];
    setValues(next);
  }
  async function save(e: React.FormEvent) {
    e.preventDefault();
    let body: RecordValue;
    try {
      body = settingPayload(section, values, connectors?.items ?? []);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "请检查填写内容");
      return;
    }
    setBusy(true);
    try {
      if (record?.id)
        await api.updateSetting(config.resource, String(record.id), {
          ...body,
          expectedVersion: record.version ?? 0,
        });
      else await api.createSetting(config.resource, body);
      toast.success("配置已保存");
      setRecord(null);
      await mutate();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  }
  async function removeSetting() {
    if (!deleteRecord?.id) return;
    const id = String(deleteRecord.id);
    setDeleting(id);
    try {
      const latest = await mutate();
      const current = (latest?.items ?? data?.items ?? []).find(
        (item) => String((item as RecordValue).id) === id,
      ) as RecordValue | undefined;
      if (!current) {
        toast.success("配置已删除");
        setDeleteRecord(null);
        return;
      }
      await api.deleteSetting(config.resource, id, {
        expectedVersion: current.version ?? deleteRecord.version ?? 0,
        force: forceDelete,
      });
      toast.success("配置已删除");
      setDeleteRecord(null);
      await mutate();
    } catch (err) {
      reportError(err);
    } finally {
      setDeleting("");
    }
  }
  async function extra(r: RecordValue) {
    setRunning(String(r.id));
    try {
      const result = await api.settingAction(
        config.resource,
        String(r.id),
        config.action ?? "sync",
        { expectedVersion: Number(r.version ?? 0) },
      );
      if (result && "message" in result)
        toast.success(result.message || "任务已提交");
      else toast.success("任务已提交");
      await mutate();
    } catch (e) {
      reportError(e);
    } finally {
      setRunning("");
    }
  }
  function renderFields(items: SettingField[]) {
    return items.filter((f) => !(section === "connections" && record?.id && f.key === "provider")).map((f, index, visibleFields) => (
      <Fragment key={f.key}>
        {f.group && f.group !== visibleFields[index - 1]?.group && (
          <h3 className="border-t border-border pt-4 text-xs font-semibold text-muted-foreground">{f.group}</h3>
        )}
        <Field
          key={f.key}
          label={`${f.label}${f.required ? " *" : ""}`}
          hint={f.hint}
          group={f.kind === "lines" && Boolean(f.lookup || f.options)}
        >
          {f.kind === "boolean" ? (
            <Switch.Root
              checked={Boolean(values[f.key])}
              onCheckedChange={(v) =>
                changeValue(f.key, v)
              }
              className="relative h-6 w-10 rounded-full bg-stone-200 transition data-[state=checked]:bg-primary"
            >
              <Switch.Thumb className="block size-4 translate-x-1 rounded-full bg-white shadow transition data-[state=checked]:translate-x-5" />
            </Switch.Root>
          ) : f.kind === "lines" && (f.lookup || f.options) ? (
            <div className="max-h-40 space-y-2 overflow-y-auto rounded-lg border border-border p-3">
              {options(f).length ? (
                options(f).map((o: { value: string; label: string }) => (
                  <label
                    key={o.value}
                    className="flex items-center gap-2 text-xs"
                  >
                    <input
                      type="checkbox"
                      className="accent-primary"
                      checked={
                        Array.isArray(values[f.key]) &&
                        (values[f.key] as string[]).includes(o.value)
                      }
                      onChange={(e) => {
                        const existing = Array.isArray(values[f.key])
                          ? (values[f.key] as string[])
                          : [];
                        changeValue(f.key, e.target.checked
                          ? [...existing, o.value]
                          : existing.filter((v) => v !== o.value));
                      }}
                    />
                    {o.label}
                  </label>
                ))
              ) : (
                <p className="text-xs text-muted-foreground">
                  暂无选项
                </p>
              )}
            </div>
          ) : f.lookup ? (
            <SearchSelect
              value={String(values[f.key] ?? "")}
              onChange={(v) => changeValue(f.key, v)}
              options={[
              ...(f.required ? [] : [{ value: "", label: f.key === "notificationChannelId" ? "使用值班通知渠道" : section === "connections" && f.key === "repositoryId" ? "手动填写仓库" : "不指定" }]),
                ...options(f),
              ]}
            placeholder={f.key === "notificationChannelId" ? "使用值班通知渠道" : section === "connections" && f.key === "repositoryId" ? "手动填写仓库" : `选择${f.label}`}
            />
          ) : f.kind === "select" ? (
            <Select
              value={String(values[f.key] ?? "")}
              onChange={(e) =>
                changeValue(f.key, e.target.value)
              }
              required={f.required}
            >
              {values[f.key] &&
                !f.options?.some((o) => o.value === values[f.key]) ? (
                <option value={String(values[f.key])}>
                  {label(String(values[f.key]))}{section === "channels" && f.key === "replyMode" ? "（此平台不支持，请重新选择）" : ""}
                </option>
              ) : null}
              {f.options?.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </Select>
          ) : f.kind === "textarea" ||
            f.kind === "lines" ||
            f.kind === "json" ? (
            <Textarea
              value={
                Array.isArray(values[f.key])
                  ? (values[f.key] as string[]).join("\n")
                  : String(values[f.key] ?? "")
              }
              onChange={(e) =>
                changeValue(f.key, f.kind === "lines"
                  ? e.target.value.split("\n") : e.target.value)
              }
              placeholder={f.placeholder}
              required={f.required}
              className="min-h-20 font-mono text-xs"
            />
          ) : (
            <Input
              type={
                section === "policies" && f.key === "expiresAt" ? "datetime-local" : f.kind === "password"
                  ? "password"
                  : f.kind === "number"
                    ? "number"
                    : "text"
              }
              autoComplete={
                f.kind === "password" ? "new-password" : "off"
              }
              value={String(values[f.key] ?? "")}
              onChange={(e) =>
                changeValue(f.key, e.target.value)
              }
              placeholder={
                f.kind === "password" && record?.id
                  ? "留空保持不变"
                  : f.placeholder
              }
              required={f.required}
              min={f.kind === "number" ? 0 : undefined}
            />
          )}
        </Field>
      </Fragment>
    ));
  }

  return (
    <>
      <PageHeader
        title="设置"
      />
      <nav aria-label="设置分类" className="mb-6 grid gap-3 lg:grid-cols-[1fr_2fr_2fr]">
        {navigationGroups.map((group) => (
          <div
            key={group.label}
            role="group"
            aria-label={group.label}
            className="min-w-0 rounded-lg border border-border bg-card p-3"
          >
            <p className="mb-2 flex items-center gap-2 px-1 text-xs font-medium text-muted-foreground">
              <group.icon aria-hidden="true" className="size-3.5" />
              {group.label}
            </p>
            <div className="flex flex-wrap gap-1">
              {group.links.map((link) => (
                <Link
                  key={link.id}
                  href={`/settings/${link.id}`}
                  aria-current={section === link.id ? "page" : undefined}
                  className={cn(
                    "inline-flex min-h-9 flex-1 items-center justify-center whitespace-nowrap rounded-md border px-2 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
                    section === link.id
                      ? "border-primary/15 bg-primary/8 font-medium text-primary"
                      : "border-transparent text-muted-foreground hover:bg-muted hover:text-foreground",
                  )}
                >
                  {link.label}
                </Link>
              ))}
            </div>
          </div>
        ))}
      </nav>
      {section === "model" ? (
        <ModelSettingsPanel />
      ) : section === "execution" ? (
        <ExecutionSettingsPanel />
      ) : config ? (
        <Panel
          title={config.title}
          action={
            admin && (section === "connections" ? (
              <div className="flex flex-wrap gap-2">
                <Button variant="outline" size="sm" onClick={() => edit({ provider: "feishu" })}>
                  <Plus className="size-3.5" />飞书 / Lark
                </Button>
                <Button variant="outline" size="sm" onClick={() => edit({ provider: "github" })}>
                  <Plus className="size-3.5" />GitHub
                </Button>
              </div>
            ) : (
              <Button size="sm" onClick={() => edit({})}>
                <Plus className="size-3.5" />新增
              </Button>
            ))
          }
        >
          {!admin && (
            <div className="border-b border-border bg-muted/50 px-5 py-3 text-xs text-muted-foreground">
              只读
            </div>
          )}
          {isLoading ? (
            <Loading />
          ) : error ? (
            <ErrorState error={error} retry={() => void mutate()} />
          ) : data?.items?.length ? (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead className="border-b border-border bg-muted/35 text-sm text-muted-foreground">
                  <tr>
                    {config.columns.map((c) => (
                      <th key={c.key} className="px-5 py-3 font-medium">
                        {c.label}
                      </th>
                    ))}
                    <th className="px-5 py-3 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {data.items.map((raw) => {
                    const r = raw as unknown as RecordValue;
                    return (
                      <tr key={String(r.id)} className="table-row">
                        {config.columns.map((c) => (
                          <td
                            key={c.key}
                            className={c.key === "lastError" ? "min-w-56 max-w-md whitespace-normal break-words px-5 py-4 text-red-700" : c.key === "id" && section === "runtimeMachines" ? "min-w-96 px-5 py-4" : "max-w-64 truncate px-5 py-4"}
                          >
                            {section === "policies" && c.key === "notificationEnabled" ? (r.notificationEnabled === false ? "关闭" : "开启") : section === "policies" && c.key === "scope" ? (
                              r.kind === "create_task" ? `${channels?.items?.find(channel => channel.id === r.channelId)?.name || "渠道已移除"} → ${repos?.items?.find(repo => repo.id === r.repositoryId)?.name || "仓库已移除"}` : r.kind === "reply_send" ? (Array.isArray(r.channelIds) && r.channelIds.length ? r.channelIds.map(id => channels?.items?.find(channel => channel.id === id)?.name || "渠道已移除").join("、") : "全部渠道") : r.kind === "knowledge_publish" ? "全部知识" : (Array.isArray(r.repositoryIds) && r.repositoryIds.length ? r.repositoryIds.map(id => repos?.items?.find(repo => repo.id === id)?.name || "仓库已移除").join("、") : "全部仓库")
                            ) : section === "policies" && c.key === "enabled" && r.enabled && r.expiresAt && new Date(String(r.expiresAt)).getTime() <= Date.now() ? "已过期" : ["status", "mode", "replyMode", "role"].includes(
                              c.key,
                            ) ? (
                              <Status value={String(r[c.key] ?? "")} />
                            ) : c.key === "wsStatus" ? (
                              r.provider === "github" ? "—" : !r.wsEnabled ? "未启用" : !r.wsCheckedAt || Date.now() - new Date(String(r.wsCheckedAt)).getTime() > 45000 ? "连接状态未知" : r.wsStatus === "connected" ? "已连接" : r.wsStatus === "error" ? String(r.wsError || "连接失败") : "连接中"
                            ) : c.key === "pollIntervalMinutes" ? (
                              !["github", "feishu", "lark"].includes(connectors?.items?.find((item) => item.id === r.connectorId)?.provider ?? "")
                                ? "—"
                                : Number(r[c.key] ?? 0) > 0 ? `每 ${r[c.key]} 分钟` : "关闭"
                            ) : c.key.endsWith("At") ? (
                              <Time value={r[c.key] as string} />
                            ) : typeof r[c.key] === "boolean" ? (
                              <span
                                className={cn(
                                  "inline-flex items-center gap-1.5",
                                  r[c.key]
                                    ? "text-primary"
                                    : "text-muted-foreground",
                                )}
                              >
                                {r[c.key] ? <Check className="size-3" /> : null}
                                {c.key === "secretConfigured"
                                  ? r[c.key]
                                    ? "已配置"
                                    : "未配置"
                                  : r[c.key]
                                    ? "已启用"
                                    : "已停用"}
                              </span>
                            ) : [
                              "primaryId",
                              "backupId",
                              "principalId",
                            ].includes(c.key) ? (
                              people?.items?.find((p) => p.id === r[c.key])
                                ?.name || String(r[c.key] || "—")
                            ) : c.key === "id" && section === "runtimeMachines" ? (
                              <div className="flex items-center gap-2">
                                <code className="select-all rounded bg-muted px-2 py-1 font-mono text-[11px] text-muted-foreground">{String(r.id || "—")}</code>
                                {typeof r.id === "string" && <Button type="button" variant="ghost" size="sm" onClick={() => void navigator.clipboard.writeText(String(r.id)).then(() => toast.success("机器 ID 已复制"), () => toast.error("复制失败，请手动选择 ID"))}><Copy className="size-3.5" />复制</Button>}
                              </div>
                            ) : c.key === "provider" ? (
                              platformNames[String(r[c.key])] || String(r[c.key] || "—")
                            ) : c.key === "kind" ? (
                              config.fields.find((field) => field.key === c.key)?.options?.find((option) => option.value === r[c.key])?.label || String(r[c.key] || "—")
                            ) : (
                              String(r[c.key] || "—")
                            )}
                          </td>
                        ))}
                        <td className="px-5 py-3">
                          <div className="flex justify-end gap-1">
                            {section === "connections" && (
                              <Button
                                variant="ghost"
                                size="sm"
                                onClick={() => setCapabilityId(String(r.id))}
                              >
                                <Info className="size-3.5" />
                                检查结果
                              </Button>
                            )}
                            {config.action && admin && (
                              <Button
                                variant="ghost"
                                size="sm"
                                disabled={!!running}
                                onClick={() => void extra(r)}
                              >
                                {running === r.id ? (
                                  <Loader2 className="size-3.5 animate-spin" />
                                ) : (
                                  <RefreshCw className="size-3.5" />
                                )}
                                {config.action === "verify" ? "检查接入" : "同步"}
                              </Button>
                            )}
                            {admin && (
                              <Button
                                variant="ghost"
                                size="sm"
                                onClick={() => edit(r)}
                              >
                                <FilePenLine className="size-3.5" />
                                编辑
                              </Button>
                            )}
                            {admin && (
                              <Button
                                variant="destructive"
                                size="sm"
                                disabled={!!deleting}
                                onClick={() => setDeleteRecord(r)}
                              >
                                {deleting === r.id ? (
                                  <Loader2 className="size-3.5 animate-spin" />
                                ) : (
                                  <Trash2 className="size-3.5" />
                                )}
                                删除
                              </Button>
                            )}
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty
              title={`还没有${config.title}`}
              action={
                admin && section !== "connections" && (
                  <Button variant="outline" size="sm" onClick={() => edit({})}>
                    <Plus className="size-3.5" />
                    添加配置
                  </Button>
                )
              }
            />
          )}
        </Panel>
      ) : (
        <Empty title="未找到设置项" />
      )}
      <Dialog
        open={capabilityId !== null}
        onOpenChange={(open) => {
          if (!open) setCapabilityId(null);
        }}
        title={`${capabilityRecord?.name || "平台账号"} · 检查结果`}
      >
        {capabilityRecord ? (
          <ConnectionCapabilities
            record={capabilityRecord}
            channelNames={
              new Map(
                (channels?.items ?? []).map((channel) => [
                  channel.id ?? "",
                  channel.name || channel.id || "",
                ]),
              )
            }
          />
        ) : (
          <Empty title="无法加载账号，请刷新页面" />
        )}
      </Dialog>
      {config && (
        <>
          <Dialog
            open={record !== null}
            onOpenChange={(o) => {
              if (!o) setRecord(null);
            }}
            title={section === "connections"
              ? `${record?.id ? "编辑" : "添加"}${providerName}账号`
              : `${record?.id ? "编辑" : "新增"}${config.title}`}
          >
            <form onSubmit={save} className="space-y-4">
              {renderFields(fields.filter((field) => !field.advanced))}
              {fields.some((field) => field.advanced) && (
                <details className="rounded-lg border border-border p-4">
                  <summary className="cursor-pointer text-sm font-medium">高级设置</summary>
                  <div className="mt-4 space-y-4">{renderFields(fields.filter((field) => field.advanced))}</div>
                </details>
              )}
              <div className="flex justify-end gap-2 border-t border-border pt-4">
                <Button
                  variant="ghost"
                  type="button"
                  onClick={() => setRecord(null)}
                >
                  取消
                </Button>
                <Button type="submit" disabled={busy}>
                  {busy && <Loader2 className="size-4 animate-spin" />}保存
                </Button>
              </div>
            </form>
          </Dialog>
          <Dialog
            open={deleteRecord !== null}
            onOpenChange={(open) => {
              if (!open && !deleting) {
                setDeleteRecord(null);
                setForceDelete(false);
              }
            }}
            title={`删除${config.title}`}
            description="删除后无法恢复。"
          >
            <div className="space-y-4">
              <p className="text-sm leading-6 text-muted-foreground">
                确认删除“{String(deleteRecord?.name ?? deleteRecord?.email ?? deleteRecord?.id ?? "此配置")}”？
              </p>
              <label className="flex items-start gap-2 rounded-lg border border-red-200 bg-red-50/60 p-3 text-sm text-red-800">
                <input
                  type="checkbox"
                  className="mt-1 accent-red-600"
                  checked={forceDelete}
                  onChange={(event) => setForceDelete(event.target.checked)}
                  disabled={!!deleting}
                />
                <span>
                  强制删除，同时解除渠道授权、值班通知和审批策略等关联。历史记录保留。
                </span>
              </label>
              <div className="flex justify-end gap-2 border-t border-border pt-4">
                <Button variant="ghost" type="button" onClick={() => { setDeleteRecord(null); setForceDelete(false); }} disabled={!!deleting}>
                  取消
                </Button>
                <Button variant="destructive" type="button" disabled={!!deleting} onClick={() => void removeSetting()}>
                  {deleting && <Loader2 className="size-4 animate-spin" />}{forceDelete ? "确认强制删除" : "确认删除"}
                </Button>
              </div>
            </div>
          </Dialog>
        </>
      )}
    </>
  );
}
