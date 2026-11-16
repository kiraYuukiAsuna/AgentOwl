"use client";
import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { ArrowRight, Loader2 } from "lucide-react";
import { z } from "zod";
import useSWR, { mutate } from "swr";
import { safeReturnPath } from "@/lib/access";
import { api } from "@/lib/api/client";
import { OwlMark } from "@/components/app-shell";
import { Button } from "@/components/ui/button";
import { Input, Field } from "@/components/ui/input";
import { errorMessage, ErrorState, Loading } from "@/components/common";
export default function LoginPage() {
  return <Suspense fallback={<Loading />}><LoginForm /></Suspense>;
}
function LoginForm() {
  const query = useSearchParams();
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const {
    data: config,
    error: configError,
    isLoading,
    mutate: refreshConfig,
  } = useSWR("auth-config", api.authConfig, { shouldRetryOnError: false });
  async function login(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const values = Object.fromEntries(new FormData(e.currentTarget));
    const parsed = z
      .object({
        email: z.email("请输入正确的邮箱地址"),
        password: z.string().min(1, "请输入密码"),
      })
      .safeParse(values);
    if (!parsed.success) {
      setError(parsed.error.issues[0].message);
      return;
    }
    setBusy(true);
    setError("");
    try {
      const user = await api.login(parsed.data);
      await mutate("me", user, { revalidate: false });
      const next = new URLSearchParams(window.location.search).get("next");
      router.replace(safeReturnPath(next));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="flex min-h-dvh items-center justify-center px-5 py-10">
      <div className="w-full max-w-md">
        <div className="mb-7 flex items-center gap-3">
          <OwlMark className="size-9 text-primary" />
          <span className="text-base font-semibold">AgentOwl</span>
        </div>
        <section className="rounded-xl border border-border bg-card p-6 shadow-[0_12px_40px_-24px_rgb(15_23_42/0.25)] sm:p-8">
          <h1 className="text-2xl font-semibold tracking-tight">登录</h1>
          {query.get("login_error") && <p role="alert" className="mt-4 text-sm text-red-700">{query.get("login_error")}</p>}
          {isLoading ? (
            <Loading text="正在加载登录方式…" />
          ) : configError ? (
            <ErrorState
              error={configError}
              retry={() => void refreshConfig()}
            />
          ) : config?.mode === "oidc" ? (
            <div className="mt-6">
              <Button className="w-full" size="lg" asChild>
                <a href="/auth/login">
                  使用团队账号登录
                  <ArrowRight className="size-4" />
                </a>
              </Button>
            </div>
          ) : config?.mode === "local" ? (
            <form onSubmit={login} className="mt-6 space-y-4">
              <Field label="邮箱">
                <Input
                  name="email"
                  type="email"
                  autoComplete="username"
                  placeholder="you@company.com"
                  required
                  disabled={busy}
                />
              </Field>
              <Field label="密码">
                <Input
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  placeholder="输入登录密码"
                  required
                  disabled={busy}
                />
              </Field>
              {error && (
                <p
                  role="alert"
                  className="rounded-lg bg-red-50 p-3 text-xs leading-relaxed text-red-700"
                >
                  {error}
                </p>
              )}
              <Button
                disabled={busy}
                className="mt-2 w-full"
                size="lg"
                type="submit"
              >
                {busy ? (
                  <Loader2 className="size-4 animate-spin" />
                ) : (
                  <>
                    登录
                    <ArrowRight className="size-4" />
                  </>
                )}
              </Button>
            </form>
          ) : (
            <ErrorState
              error={new Error("无法加载登录方式，请重试")}
              retry={() => void refreshConfig()}
            />
          )}
          {!!config?.larkApps?.length && (
            <div className="mt-6 space-y-2 border-t border-border pt-6">
              {config.larkApps.map(app => (
                <Button key={app.id} variant="outline" className="w-full" size="lg" asChild>
                  <a href={`/auth/lark/login?connector_id=${encodeURIComponent(app.id ?? "")}`}>
                    {app.provider === "lark" ? "Lark 登录" : "飞书登录"}
                    {config.larkApps!.length > 1 && <span className="text-xs text-muted-foreground">{app.name}</span>}
                  </a>
                </Button>
              ))}
            </div>
          )}
          <p className="mt-6 text-center text-xs leading-6 text-muted-foreground">
            无法登录？请联系管理员。
          </p>
        </section>
      </div>
    </main>
  );
}
