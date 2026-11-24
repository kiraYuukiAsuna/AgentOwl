"use client";
import { useState } from "react";
import { Download, Loader2, Paperclip } from "lucide-react";
import { api } from "@/lib/api/client";
import { attachmentFilename, attachmentReferences } from "@/lib/attachments";
import { downloadBinary } from "@/lib/download";
import { Panel, reportError } from "@/components/common";
import { Button } from "@/components/ui/button";

export function CaseAttachments({
  caseId,
  version,
  references,
}: {
  caseId: string;
  version: number;
  references: unknown;
}) {
  const [busy, setBusy] = useState<number | null>(null);
  const items = attachmentReferences(references);
  if (!items.length) return null;
  return (
    <Panel
      title="消息附件"
    >
      <p className="border-b border-border bg-muted/40 px-5 py-3 text-xs leading-6 text-muted-foreground">
        下载前请留意附件中的敏感信息，单文件下载上限为 4 MiB。
      </p>
      <div className="divide-y divide-border">
        {items.map((item) => (
          <div className="flex items-start gap-3 p-5" key={item.index}>
            <Paperclip className="mt-1 size-4 shrink-0 text-muted-foreground" />
            <div className="min-w-0 flex-1">
              <p className="break-all text-sm font-medium">{item.name}</p>
              <p className="mt-1 text-xs text-muted-foreground">
                {item.type === "image"
                  ? "图片"
                  : item.type === "file"
                    ? "文件"
                    : "未知格式"}
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={!item.readable || busy !== null}
              onClick={async () => {
                setBusy(item.index);
                try {
                  const content = await api.caseAttachmentContent(
                    caseId,
                    item.index,
                    version,
                  );
                  if (!content) throw new Error("附件下载失败，原消息中的文件可能已过期或被删除");
                  downloadBinary(
                    content,
                    attachmentFilename(item.name, item.index),
                  );
                } catch (error) {
                  reportError(error);
                } finally {
                  setBusy(null);
                }
              }}
            >
              {busy === item.index ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Download className="size-3.5" />
              )}
              下载原件
            </Button>
          </div>
        ))}
      </div>
    </Panel>
  );
}
