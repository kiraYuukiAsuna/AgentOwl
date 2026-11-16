import Link from "next/link";
import { ArrowUpRight, MessageSquare } from "lucide-react";
import type { CaseEscaped as SupportCase } from "@/lib/api/generated/models";
import { Empty, label, Priority, Status, Time } from "@/components/common";
import { caseSourceLabel } from "@/lib/activity-labels";
import { shortId } from "@/lib/utils";
export function CaseTable({ items }: { items: SupportCase[] }) {
  if (!items.length)
    return (
      <Empty
        title="暂无问题"
      />
    );
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-sm">
        <thead className="border-b border-border bg-muted/35 text-sm font-medium text-muted-foreground">
          <tr>
            <th className="w-20 whitespace-nowrap px-5 py-3 font-medium">优先级</th>
            <th className="py-3 font-medium">问题</th>
            <th className="px-4 py-3 font-medium">状态</th>
            <th className="hidden px-4 py-3 font-medium lg:table-cell">模块 / 类型</th>
            <th className="px-5 py-3 text-right font-medium">最近更新</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {items.map((item) => (
            <tr key={item.id} className="table-row">
              <td className="px-5 py-4">
                <Priority value={item.priority} />
              </td>
              <td className="min-w-60 py-4">
                <Link
                  href={`/cases/${item.id}`}
                  className="group flex items-start gap-2.5 pr-3"
                >
                  <MessageSquare className="mt-1 size-4 shrink-0 text-muted-foreground/65" />
                  <div className="min-w-0">
                    <p className="line-clamp-1 text-sm font-medium group-hover:text-primary">
                      {item.title}
                      <ArrowUpRight className="ml-1 inline size-3 opacity-0 group-hover:opacity-100" />
                    </p>
                    <div className="mt-1.5 flex items-center gap-2 text-xs text-muted-foreground">
                      <span className="font-mono">OWL-{shortId(item.id)}</span>
                      <span>·</span>
                      <span>{caseSourceLabel(item.source as string)}</span>
                    </div>
                  </div>
                </Link>
              </td>
              <td className="px-4 py-4">
                <Status value={item.status} />
              </td>
              <td className="hidden px-4 py-4 lg:table-cell">
                <span className="text-xs text-muted-foreground">
                  {item.module || "未分类"} / {label(item.category || "unknown")}
                </span>
              </td>
              <td className="px-5 py-4 text-right">
                <Time value={item.updatedAt} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
