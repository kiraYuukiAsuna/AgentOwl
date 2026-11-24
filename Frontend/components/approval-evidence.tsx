"use client";
import useSWR from "swr";
import { BookOpen, Code2, MessageSquare } from "lucide-react";
import type { Approval } from "@/lib/api/generated/models";
import { api } from "@/lib/api/client";
import { objectData } from "@/lib/utils";
import { approvalExecutionDetails, formatTaskQuota } from "@/lib/workflow";
import { Definition, label, Status } from "@/components/common";

export function ApprovalEvidence({ approval }: { approval: Approval }) {
  const data = objectData(approval.action);
  const { data: repos } = useSWR(
    ["step", "admission"].includes(approval.subjectType ?? "") ? "repositories" : null,
    () => api.repositories({ limit: 100 }),
  );
  if (approval.kind === "create_task") return <div className="space-y-4">
    <h2 className="font-semibold">创建开发任务</h2>
    <div className="rounded-lg border border-border px-4">
      <Definition label="开发仓库" value={repos?.items?.find(repo => repo.id === data.repository_id)?.name || "待选择"} />
      <Definition label="任务目标" value={String(data.goal || "待补充")} />
      <Definition label="验收条件" value={String(data.acceptance_criteria || "待补充")} />
      <Definition label="允许修改" value={Array.isArray(data.allowed_paths) ? data.allowed_paths.join("、") : "—"} />
      <Definition label="后续补充" value={data.followup_mode === "within_scope" ? "自动应用范围内补充" : "手动确认后应用"} />
    </div>
    {data.reason ? <p className="text-sm text-muted-foreground">{String(data.reason)}</p> : null}
  </div>;
  if (approval.subjectType === "knowledge")
    return (
      <div className="space-y-5">
        <div className="flex items-start gap-3">
          <span className="rounded-lg bg-primary/5 p-2.5 text-primary">
            <BookOpen className="size-4" />
          </span>
          <div>
            <h2 className="text-base font-semibold">
              {String(data.title || "发布到知识库")}
            </h2>
          </div>
        </div>
        <div className="rounded-lg border border-border px-4">
          <Definition
            label="发布范围"
            value={<Status value={String(data.visibility ?? "")} />}
          />
        </div>
        <div className="rounded-lg bg-muted/60 p-5">
          <p className="prose-body">{String(data.body ?? "未填写正文")}</p>
        </div>
      </div>
    );
  if (approval.subjectType === "reply") {
    const images = Array.isArray(data.image_evidence)
      ? data.image_evidence.map((item) => objectData(item as { additionalData?: Record<string, unknown> }))
      : [];
    return (
      <div className="space-y-5">
        <div className="flex items-center gap-3">
          <span className="rounded-lg bg-primary/5 p-2.5 text-primary">
            <MessageSquare className="size-4" />
          </span>
          <div>
            <h2 className="text-base font-semibold">发送回复</h2>
          </div>
        </div>
        <div className="rounded-lg bg-muted/60 p-5">
          <p className="prose-body">{String(data.body ?? "未填写回复")}</p>
        </div>
        <div className="rounded-lg border border-border px-4">
          <Definition
            label="发送渠道"
            value={
              <span className="font-mono">
                {String(data.channel_id ?? "—")}
              </span>
            }
          />
          <Definition
            label="引用知识"
            value={`${Array.isArray(data.evidence) ? data.evidence.length : 0} 条`}
          />
          <Definition
            label="引用图片"
            value={`${images.length} 张`}
          />
        </div>
        {images.length > 0 && (
          <div>
            <h3 className="mb-3 text-xs font-medium text-muted-foreground">引用图片</h3>
            <div className="flex flex-wrap gap-2">
              {images.map((image, index) => (
                <span className="rounded-md bg-muted px-2 py-1 font-mono text-[10px] text-muted-foreground" key={String(image.id ?? index)}>
                  {String(image.name ?? image.id ?? `image-${index + 1}`)}
                </span>
              ))}
            </div>
          </div>
        )}
      </div>
    );
  }
  if (approval.subjectType === "step") {
    const manifest = data.context_manifest as { facts?: { requester?: { name?: string; subject_id?: string; principal_id?: string }; sponsor?: { name?: string }; conversation?: { private?: boolean; channel_id?: string } }; refs?: { id: string; title: string }[] } | undefined;
    const input = data.input as { previous_summary?: string; context_documents?: { previous_summary?: string } } | undefined;
    const previousResult = input?.previous_summary || input?.context_documents?.previous_summary || data.context;
    const paths = Array.isArray(data.changed_paths)
      ? data.changed_paths.filter((p): p is string => typeof p === "string")
      : [];
    const repo = repos?.items?.find((r) => r.id === data.repository_id);
    return (
      <div className="space-y-5">
        <div className="flex items-center gap-3">
          <span className="rounded-lg bg-primary/5 p-2.5 text-primary">
            <Code2 className="size-4" />
          </span>
          <div>
            <h2 className="text-base font-semibold">
              {label(String(data.stage ?? approval.kind ?? ""))}
            </h2>
            <p className="mt-1 text-xs text-muted-foreground">
              版本 {String(data.revision ?? "—")} · 第{" "}
              {String(data.epoch ?? "—")} 轮执行
            </p>
          </div>
        </div>
        <div className="rounded-lg border border-border px-4">
          <Definition
            label="代码仓库"
            value={repo?.name || String(data.repository_id ?? "—")}
          />
          <Definition label="文件数量" value={String(data.file_count ?? "—")} />
          {typeof data.base_commit === "string" && (
            <Definition
              label="起始提交"
              value={
                <span className="font-mono break-all">{data.base_commit}</span>
              }
            />
          )}
          {typeof data.runner_image === "string" && (
            <Definition
              label="运行环境镜像"
              value={
                <span className="font-mono break-all">{data.runner_image}</span>
              }
            />
          )}
          <Definition
            label="测试结果"
            value={
              data.tests_passed === true ? (
                <span className="text-primary">测试通过</span>
              ) : (
                "暂无通过记录"
              )
            }
          />
          {approvalExecutionDetails(data).map((item) => <Definition key={item.label} label={item.label} value={item.value} />)}
          <Definition
            label="模型 Token 上限"
            value={formatTaskQuota(data.max_tokens, "Token")}
          />
        </div>
        {typeof data.goal === "string" && data.goal && (
          <div>
            <h3 className="mb-3 text-xs font-medium text-muted-foreground">
              本次目标
            </h3>
            <p className="rounded-lg bg-muted/60 p-4 prose-body">{data.goal}</p>
          </div>
        )}
        {manifest?.facts && <div className="rounded-lg border border-border px-4">
          <Definition label="请求人" value={manifest.facts.requester?.name || manifest.facts.requester?.subject_id || "未关联成员"} />
          <Definition label="任务授权人" value={manifest.facts.sponsor?.name || "—"} />
          <Definition label="参考资料" value={manifest.refs?.map(ref => ref.title).join("、") || "—"} />
        </div>}
        {typeof previousResult === "string" && previousResult && (
          <div>
            <h3 className="mb-3 text-xs font-medium text-muted-foreground">
              方案与结果
            </h3>
            <p className="max-h-96 overflow-auto rounded-lg bg-muted/60 p-4 prose-body">
              {previousResult}
            </p>
          </div>
        )}
        {Array.isArray(data.allowed_paths) && (
          <div>
            <h3 className="mb-3 text-xs font-medium text-muted-foreground">
              允许修改的路径
            </h3>
            <div className="flex flex-wrap gap-2">
              {data.allowed_paths
                .filter((p): p is string => typeof p === "string")
                .map((path) => (
                  <code
                    key={path}
                    className="rounded-md bg-muted px-2 py-1 text-xs"
                  >
                    {path}
                  </code>
                ))}
            </div>
          </div>
        )}
        {Array.isArray(data.test_argv) && data.test_argv.length > 0 && (
          <div>
            <h3 className="mb-3 text-xs font-medium text-muted-foreground">
              测试命令与参数
            </h3>
            <pre className="overflow-auto rounded-lg bg-muted px-4 py-3 font-mono text-xs leading-6">
              {JSON.stringify(data.test_argv, null, 2)}
            </pre>
          </div>
        )}
        {typeof data.test_digest === "string" && data.test_digest && (
          <div className="text-xs text-muted-foreground">
            测试代码快照摘要{" "}
            <code className="mt-2 block break-all">{data.test_digest}</code>
          </div>
        )}
        <div>
          <h3 className="mb-3 text-xs font-medium text-muted-foreground">
            修改的文件 · {paths.length} 项
          </h3>
          {paths.length ? (
            <div className="max-h-52 overflow-auto rounded-lg bg-muted px-4 py-3 font-mono text-xs leading-7">
              {paths.map((path) => (
                <p key={path} className="break-all">
                  {path}
                </p>
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">
              暂无文件变更
            </p>
          )}
        </div>
      </div>
    );
  }
  return null;
}
