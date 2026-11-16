"use client";
import { useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import useSWR from "swr";
import {
  BookOpen,
  Globe2,
  Plus,
  Search,
  Settings2,
  Shield,
} from "lucide-react";
import { toast } from "sonner";
import { hasRole } from "@/lib/access";
import { api, useMe } from "@/lib/api/client";
import {
  PageHeader,
  Panel,
  ErrorState,
  Loading,
  Empty,
  Status,
  Time,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Input, Select } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import { KnowledgeEditor } from "@/components/knowledge-editor";
export default function KnowledgePage() {
  const router = useRouter();
  const { data: me } = useMe();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [cursor, setCursor] = useState("");
  const [open, setOpen] = useState(false);
  const { data, error, isLoading, mutate } = useSWR(
    ["knowledge", search, status, cursor],
    () => api.knowledge({ q: search, status, cursor, limit: 24 }),
  );
  const canWrite = hasRole(me, "knowledge", "developer");
  return (
    <>
      <PageHeader
        title="知识库"
        action={
          <>
            <Button variant="outline" asChild>
              <Link href="/settings/sources">
                <Settings2 className="size-4" />
                知识来源
              </Link>
            </Button>
            {canWrite && (
              <Button onClick={() => setOpen(true)}>
                <Plus className="size-4" />
                新建知识
              </Button>
            )}
          </>
        }
      />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <div className="relative w-full sm:w-80">
          <Search className="absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setCursor("");
            }}
            aria-label="搜索知识"
            placeholder="搜索问题、命令或排查步骤"
            className="pl-9"
          />
        </div>
        <Select
          className="w-36"
          aria-label="知识状态"
          value={status}
          onChange={(e) => {
            setStatus(e.target.value);
            setCursor("");
          }}
        >
          <option value="">全部状态</option>
          <option value="published">已发布</option>
          <option value="draft">草稿</option>
          <option value="submitted">待审批</option>
          <option value="withdrawn">已撤回</option>
        </Select>
      </div>
      {isLoading ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} retry={() => void mutate()} />
      ) : data?.items?.length ? (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {data.items.map((k) => (
            <Link
              key={k.id}
              href={`/knowledge/${k.id}`}
              className="group flex min-h-48 flex-col rounded-xl border border-border bg-card p-5 transition-colors hover:border-primary/35"
            >
              <div className="mb-4 flex items-center justify-between">
                <span className="rounded-lg bg-primary/8 p-2.5 text-primary">
                  <BookOpen className="size-4" />
                </span>
                <Status value={k.status} />
              </div>
              <h2 className="line-clamp-2 text-base leading-6 font-semibold group-hover:text-primary">
                {k.title}
              </h2>
              <p className="mt-2 line-clamp-3 text-xs leading-6 text-muted-foreground">
                {k.body?.replace(/[#*`]/g, "")}
              </p>
              <div className="mt-auto flex items-center justify-between pt-5 text-[11px] text-muted-foreground">
                <span className="inline-flex items-center gap-1">
                  {k.visibility === "public" ? (
                    <Globe2 className="size-3" />
                  ) : (
                    <Shield className="size-3" />
                  )}
                  {k.visibility === "public"
                    ? "公开"
                    : k.visibility === "channel"
                      ? "指定渠道"
                      : "团队内部"}
                  <span className="mx-1">·</span>
                  {k.module || "通用"}
                </span>
                <Time value={k.updatedAt} />
              </div>
            </Link>
          ))}
        </div>
      ) : (
        <Panel>
          <Empty
            title="暂无知识"
            action={
              canWrite && (
                <Button variant="outline" onClick={() => setOpen(true)}>
                  <Plus className="size-4" />
                  新建知识
                </Button>
              )
            }
          />
        </Panel>
      )}
      <div className="mt-5 flex items-center justify-between text-xs text-muted-foreground">
        <span>共 {data?.total ?? "—"} 条知识</span>
        <div className="flex gap-2">
          {cursor && (
            <Button variant="ghost" size="sm" onClick={() => setCursor("")}>
              返回第一页
            </Button>
          )}
          <Button
            size="sm"
            variant="outline"
            disabled={!data?.nextCursor}
            onClick={() => setCursor(data?.nextCursor ?? "")}
          >
            下一页
          </Button>
        </div>
      </div>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="新建知识"
      >
        <KnowledgeEditor
          onCancel={() => setOpen(false)}
          onSave={async (value) => {
            const result = await api.createKnowledge(value);
            toast.success("草稿已保存");
            setOpen(false);
            await mutate();
            if (result?.id) router.push(`/knowledge/${result.id}`);
          }}
        />
      </Dialog>
    </>
  );
}
