"use client";
import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import useSWR from "swr";
import { Filter, Loader2, Plus, Search, X } from "lucide-react";
import { z } from "zod";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import { CaseTable } from "@/components/case-table";
import {
  ErrorState,
  Loading,
  PageHeader,
  Panel,
  reportError,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input, Textarea, Select, Field } from "@/components/ui/input";
export default function CasesPage() {
  return (
    <Suspense fallback={<Loading />}>
      <CasesContent />
    </Suspense>
  );
}
function CasesContent() {
  const router = useRouter();
  const { data: me } = useMe();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [cursor, setCursor] = useState("");
  const params = useSearchParams();
  const [createOpen, setCreateOpen] = useState(false);
  const open = createOpen || params.get("new") === "1";
  function setOpen(value: boolean) {
    setCreateOpen(value);
    if (!value && params.has("new")) router.replace("/cases");
  }
  const [busy, setBusy] = useState(false);
  const { data, error, isLoading, mutate } = useSWR(
    ["cases", search, status, cursor],
    () => api.cases({ q: search, status, cursor, limit: 30 }),
    { refreshInterval: 15000 },
  );
  async function create(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const p = z
      .object({
        title: z.string().trim().min(3, "标题至少 3 个字"),
        description: z.string().trim().min(1, "请描述问题"),
        priority: z.string(),
        module: z.string(),
      })
      .safeParse(Object.fromEntries(new FormData(e.currentTarget)));
    if (!p.success) {
      toast.error(p.error.issues[0].message);
      return;
    }
    setBusy(true);
    try {
      const result = await api.createCase(p.data);
      toast.success("问题已创建");
      setOpen(false);
      await mutate();
      if (result?.id) router.push(`/cases/${result.id}`);
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <PageHeader
        title="问题"
        action={
          hasRole(me, "oncall", "developer") && (
            <Button onClick={() => setOpen(true)}>
              <Plus className="size-4" />
              新建问题
            </Button>
          )
        }
      />
      <Panel>
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border p-4">
          <div className="relative w-full sm:w-72">
            <Search className="absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={search}
              onChange={(e) => {
                setSearch(e.target.value);
                setCursor("");
              }}
              placeholder="搜索标题、正文或错误码"
              className="pl-9"
              aria-label="搜索问题"
            />
          </div>
          <div className="flex items-center gap-2">
            <Filter className="size-3.5 text-muted-foreground" />
            <Select
              className="w-36"
              aria-label="问题状态"
              value={status}
              onChange={(e) => {
                setStatus(e.target.value);
                setCursor("");
              }}
            >
              <option value="">全部状态</option>
              <option value="triage">待分类</option>
              <option value="open">待处理</option>
              <option value="awaiting_info">待补充</option>
              <option value="in_progress">处理中</option>
              <option value="waiting_user">待用户反馈</option>
              <option value="waiting_fix">待修复</option>
              <option value="awaiting_release">待发布</option>
              <option value="resolved">已解决</option>
              <option value="closed">已关闭</option>
            </Select>
            {(search || status) && (
              <Button
                variant="ghost"
                size="icon"
                aria-label="清空筛选"
                onClick={() => {
                  setSearch("");
                  setStatus("");
                  setCursor("");
                }}
              >
                <X className="size-4" />
              </Button>
            )}
          </div>
        </div>
        {isLoading ? (
          <Loading />
        ) : error ? (
          <ErrorState error={error} retry={() => void mutate()} />
        ) : (
          <CaseTable items={data?.items ?? []} />
        )}
        <div className="flex items-center justify-between border-t border-border px-5 py-3">
          <span className="text-xs text-muted-foreground">
            共 {data?.total ?? "—"} 个问题
          </span>
          <div className="flex gap-2">
            {cursor && (
              <Button variant="ghost" size="sm" onClick={() => setCursor("")}>
                返回第一页
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              disabled={!data?.nextCursor}
              onClick={() => setCursor(data?.nextCursor ?? "")}
            >
              下一页
            </Button>
          </div>
        </div>
      </Panel>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="新建问题"
      >
        <form onSubmit={create} className="space-y-4">
          <Field label="问题标题">
            <Input
              name="title"
              placeholder="例如：登录后调用接口仍提示 Token 失效"
              required
              minLength={3}
            />
          </Field>
          <Field label="问题描述">
            <Textarea
              name="description"
              placeholder="描述现象、环境及复现步骤（请勿包含密码或 Token）"
              required
              className="min-h-36"
            />
          </Field>
          <div className="grid grid-cols-2 gap-4">
            <Field label="优先级">
              <Select name="priority" defaultValue="P2">
                <option>P0</option>
                <option>P1</option>
                <option>P2</option>
                <option>P3</option>
              </Select>
            </Field>
            <Field label="模块">
              <Input name="module" placeholder="例如：auth、billing" />
            </Field>
          </div>
          <div className="flex justify-end gap-2 pt-2">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setOpen(false)}
            >
              取消
            </Button>
            <Button disabled={busy} type="submit">
              {busy && <Loader2 className="size-4 animate-spin" />}新建问题
            </Button>
          </div>
        </form>
      </Dialog>
    </>
  );
}
