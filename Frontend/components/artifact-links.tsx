"use client";
import { useState } from "react";
import { Download, Loader2 } from "lucide-react";
import { api } from "@/lib/api/client";
import { Button } from "@/components/ui/button";
import { reportError } from "@/components/common";
import { downloadBinary } from "@/lib/download";

export function ArtifactLinks({ data }: { data: Record<string, unknown> }) {
  const [busy, setBusy] = useState("");
  const artifacts = [
    { key: "diff_artifact_id", label: "代码变更", filename: "diff.patch" },
    { key: "result_artifact_id", label: "运行结果", filename: "result.json" },
    { key: "snapshot_id", label: "代码快照", filename: "snapshot.json" },
    { key: "test_log_artifact_id", label: "测试日志", filename: "test.log" },
    { key: "tools_artifact_id", label: "操作日志", filename: "tools.ndjson" },
  ].filter(({ key }) => typeof data[key] === "string" && data[key]);
  if (!artifacts.length) return null;
  return (
    <div className="flex flex-wrap gap-2">
      {artifacts.map(({ key, label, filename }) => (
        <Button
          key={key}
          variant="outline"
          size="sm"
          disabled={!!busy}
          onClick={async () => {
            setBusy(key);
            try {
              const content = await api.artifactContent(String(data[key]));
              if (!content) throw new Error("文件为空或暂时无法下载，请稍后重试");
              downloadBinary(content, filename);
            } catch (err) {
              reportError(err);
            } finally {
              setBusy("");
            }
          }}
        >
          {busy === key ? (
            <Loader2 className="size-3 animate-spin" />
          ) : (
            <Download className="size-3" />
          )}
          {label}
        </Button>
      ))}
    </div>
  );
}
