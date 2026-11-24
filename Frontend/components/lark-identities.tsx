"use client";
import { useState } from "react";
import { useSearchParams } from "next/navigation";
import useSWR, { mutate as refresh } from "swr";
import { toast } from "sonner";
import { runtimeRead, runtimeWrite } from "@/lib/api/client";
import { Panel, ErrorState, Loading, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";

type Binding = { id: string; version: number; provider: string; app_id: string; subject_id: string; display_name?: string; enabled?: boolean; verified_at?: string };
type Identities = { items: Binding[]; apps: { id: string; name: string; provider: string }[]; redirect_uri: string };

export function LarkIdentities() {
  const query = useSearchParams();
  const { data, error, mutate, isLoading } = useSWR("my-identities", () => runtimeRead<Identities>("/api/v1/me/identities"));
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<Binding>();
  async function bind(id: string) {
    setBusy(true);
    try {
      const result = await runtimeWrite("/api/v1/me/identities/bind", "POST", { connector_id: id }) as { url: string };
      window.location.assign(result.url);
    } catch (e) { reportError(e); setBusy(false); }
  }
  async function unbind() {
    if (!removing) return;
    setBusy(true);
    try {
      await runtimeWrite(`/api/v1/me/identities/${removing.id}`, "DELETE", { expected_version: removing.version });
      setRemoving(undefined);
      toast.success("已解除绑定");
      await refresh("me");
      await mutate();
    } catch (e) { reportError(e); } finally { setBusy(false); }
  }
  return <Panel title="飞书账号" className="mt-6"><div className="p-5">
    {query.get("login_error") && <p role="alert" className="mb-4 text-sm text-red-700">{query.get("login_error")}</p>}
    {query.has("identity_bound") && !query.has("login_error") && !!data?.items.length && <p role="status" className="mb-4 text-sm text-emerald-700">飞书账号已绑定</p>}
    {isLoading ? <Loading /> : error ? <ErrorState error={error} retry={() => void mutate()} /> : data && <div className="space-y-4">
      {data.items.length ? data.items.map(item => <div key={item.id} className="flex items-center justify-between gap-4 rounded-lg border border-border p-4">
        <div className="min-w-0"><p className="text-sm font-medium">{item.display_name || (item.provider === "lark" ? "Lark 账号" : "飞书账号")}{item.enabled === false && " · 已停用"}</p>
          <details className="mt-1 text-xs text-muted-foreground"><summary className="cursor-pointer">账号详情</summary><p className="mt-1 break-all">App ID：{item.app_id}<br />Open ID：{item.subject_id}</p></details>
        </div>
        <Button variant="outline" size="sm" disabled={busy} onClick={() => setRemoving(item)}>解除绑定</Button>
      </div>) : <p className="text-sm text-muted-foreground">暂未绑定飞书 / Lark 账号</p>}
      {data.apps.length > 0 ? <div className="flex flex-wrap gap-2">{data.apps.map(app => <Button key={app.id} variant="outline" disabled={busy} onClick={() => void bind(app.id)}>绑定 {app.name}</Button>)}</div> : <p className="text-sm text-muted-foreground">管理员尚未开启飞书应用的账号绑定功能。</p>}
      <details className="text-xs text-muted-foreground"><summary className="cursor-pointer">应用配置</summary><p className="mt-2">飞书应用回调地址</p><code className="mt-1 block break-all select-all">{data.redirect_uri}</code></details>
    </div>}
    <Dialog open={!!removing} onOpenChange={open => { if (!open && !busy) setRemoving(undefined); }} title="解除飞书绑定" description="解绑后，将无法再用此账号登录或通过卡片处理审批，现有的飞书登录会话也会立即失效。">
      <div className="flex justify-end gap-2"><Button variant="outline" disabled={busy} onClick={() => setRemoving(undefined)}>取消</Button><Button disabled={busy} onClick={() => void unbind()}>解除绑定</Button></div>
    </Dialog>
  </div></Panel>;
}
