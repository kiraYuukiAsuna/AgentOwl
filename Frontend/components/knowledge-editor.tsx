"use client";
import { useState } from "react";
import { Loader2 } from "lucide-react";
import { z } from "zod";
import { toast } from "sonner";
import useSWR from "swr";
import { api } from "@/lib/api/client";
import type { Knowledge } from "@/lib/api/generated/models";
import { Input, Textarea, Select, Field } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { reportError } from "@/components/common";
function localDateTime(value?: string | null) {
  if (!value) return "";
  const date = new Date(value);
  return Number.isNaN(date.valueOf())
    ? ""
    : new Date(date.valueOf() - date.getTimezoneOffset() * 60000)
        .toISOString()
        .slice(0, 16);
}
export function KnowledgeEditor({
  value,
  onSave,
  onCancel,
}: {
  value?: Knowledge;
  onSave: (value: Knowledge) => Promise<void>;
  onCancel: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [visibility, setVisibility] = useState(value?.visibility || "internal");
  const [channelIds, setChannelIds] = useState(value?.channelIds ?? []);
  const { data: channels } = useSWR("channels-options", () =>
    api.channels({ limit: 100 }),
  );
  return (
    <form
      className="space-y-4"
      onSubmit={async (e) => {
        e.preventDefault();
        const form = Object.fromEntries(new FormData(e.currentTarget));
        const parsed = z
          .object({
            title: z.string().trim().min(2, "标题至少 2 个字"),
            body: z.string().trim().min(1, "请填写正文"),
            module: z.string(),
            visibility: z.string(),
            cliVersion: z.string(),
            environment: z.string(),
            sourceUrl: z.union([z.literal(""), z.url("来源链接格式不正确")]),
            validUntil: z.string(),
          })
          .safeParse(form);
        if (!parsed.success) {
          toast.error(parsed.error.issues[0].message);
          return;
        }
        if (visibility === "channel" && !channelIds.length) {
          toast.error("请至少选择一个可见渠道");
          return;
        }
        setBusy(true);
        try {
          await onSave({
            ...parsed.data,
            channelIds: visibility === "channel" ? channelIds : [],
            validUntil: parsed.data.validUntil
              ? new Date(parsed.data.validUntil).toISOString()
              : "",
            expectedVersion: value?.version ?? 0,
          });
        } catch (error) {
          reportError(error);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Field label="标题">
        <Input
          name="title"
          defaultValue={value?.title ?? ""}
          placeholder="例如：登录后提示 Token 失效的排查方法"
          required
        />
      </Field>
      <div className="grid grid-cols-2 gap-4">
        <Field label="模块">
          <Input
            name="module"
            defaultValue={value?.module ?? ""}
            placeholder="token"
          />
        </Field>
        <Field label="可见范围">
          <Select
            name="visibility"
            value={visibility}
            onChange={(e) => setVisibility(e.target.value)}
          >
            <option value="internal">团队内部</option>
            <option value="public">公开</option>
            <option value="channel">指定渠道</option>
          </Select>
        </Field>
      </div>
      {visibility === "channel" && (
        <Field label="可见渠道" group>
          <div className="max-h-40 space-y-2 overflow-auto rounded-lg border border-border p-3">
            {channels?.items?.map((c) => (
              <label key={c.id} className="flex items-center gap-2 text-xs">
                <input
                  type="checkbox"
                  className="accent-primary"
                  checked={channelIds.includes(c.id ?? "")}
                  onChange={(e) =>
                    setChannelIds(
                      e.target.checked
                        ? [...channelIds, c.id ?? ""]
                        : channelIds.filter((id) => id !== c.id),
                    )
                  }
                />
                {c.name || c.externalId}
              </label>
            ))}
            {!channels?.items?.length && (
              <p className="text-xs text-muted-foreground">
                暂无可选渠道
              </p>
            )}
          </div>
        </Field>
      )}
      <Field
        label="正文"
      >
        <Textarea
          name="body"
          defaultValue={value?.body ?? ""}
          required
          className="min-h-56 font-mono text-[13px]"
          placeholder="支持 Markdown，建议包含适用条件、排查步骤和结论"
        />
      </Field>
      <div className="grid grid-cols-2 gap-4">
        <Field label="适用 CLI 版本">
          <Input
            name="cliVersion"
            defaultValue={value?.cliVersion ?? ""}
            placeholder=">= 1.0.98"
          />
        </Field>
        <Field label="环境">
          <Input
            name="environment"
            defaultValue={value?.environment ?? ""}
            placeholder="macOS / 生产"
          />
        </Field>
      </div>
      <Field label="来源链接">
        <Input
          name="sourceUrl"
          defaultValue={value?.sourceUrl ?? ""}
          placeholder="https://…"
        />
      </Field>
      <Field label="有效至（可选）">
        <Input
          name="validUntil"
          type="datetime-local"
          defaultValue={localDateTime(value?.validUntil)}
        />
      </Field>
      <div className="flex justify-end gap-2 border-t border-border pt-4">
        <Button variant="ghost" type="button" onClick={onCancel}>
          取消
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Loader2 className="size-4 animate-spin" />}
          {value?.id ? "保存新版本" : "保存草稿"}
        </Button>
      </div>
    </form>
  );
}
