"use client";
import { Suspense, useState } from "react";
import { LarkIdentities } from "@/components/lark-identities";
import { toast } from "sonner";
import useSWR from "swr";
import { Loader2 } from "lucide-react";
import { api, useMe } from "@/lib/api/client";
import { PageHeader, Panel, ErrorState, Loading, label, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/input";

export default function ProfilePage() {
  const { data: me, error, isLoading, mutate } = useMe();
  const { data: auth } = useSWR("auth-config", api.authConfig);
  const [name, setName] = useState<string | null>(null);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const draftName = name ?? me?.name ?? "";
  async function save(e: React.FormEvent) {
    e.preventDefault();
    if (!me) return;
    setBusy(true);
    try {
      await api.updateProfile({
        expectedVersion: me.version ?? 0,
        name: draftName.trim(),
        currentPassword,
        newPassword,
      });
      setCurrentPassword("");
      setNewPassword("");
      await mutate();
      setName(null);
      toast.success("个人资料已保存");
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  }
  if (isLoading) return <Loading />;
  if (error || !me) return <ErrorState error={error} retry={() => void mutate()} />;
  const localLogin = auth?.mode === "local";
  return (
    <>
      <PageHeader title="个人中心" />
      <Panel title="账号资料">
        <form onSubmit={save} className="space-y-5 p-5">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="邮箱">
              <Input value={me.email ?? ""} disabled />
            </Field>
            <Field label="角色">
              <Input value={label(me.role)} disabled />
            </Field>
          </div>
          <Field label="昵称 *">
            <Input value={draftName} onChange={(e) => setName(e.target.value)} required maxLength={200} />
          </Field>
          {localLogin ? <details className="rounded-lg border border-border p-4">
            <summary className="cursor-pointer text-sm font-medium">修改密码</summary>
            <div className="mt-4 grid gap-4 sm:grid-cols-2">
              <Field label="当前密码">
                <Input type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} disabled={!localLogin} />
              </Field>
              <Field label="新密码" hint="12–72 字节，中文占多个字节。">
                <Input type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} disabled={!localLogin} minLength={12} maxLength={72} />
              </Field>
            </div>
          </details> : <p className="text-xs text-muted-foreground">密码由团队登录平台管理。</p>}
          <div className="flex justify-end border-t border-border pt-4">
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}保存修改
            </Button>
          </div>
        </form>
      </Panel>
      <Suspense fallback={<Loading />}><LarkIdentities /></Suspense>
    </>
  );
}
