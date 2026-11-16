"use client";
import { Definition, Status, Time } from "@/components/common";
import type { Connector } from "@/lib/api/generated/models";

const capabilities = [
  ["botIdentity", "bot_identity", "机器人身份"],
  ["repositoryRead", "repository_read", "读取仓库"],
  ["receiveMentions", "receive_mentions", "接收 @ 机器人的消息"],
  ["receiveMessages", "receive_messages", "接收未 @ 机器人的消息"],
  ["historyRead", "history_read", "读取历史消息"],
  ["threadRead", "thread_read", "读取话题消息"],
  ["attachmentRead", "attachment_read", "读取附件"],
  ["send", "send", "发送和回复消息"],
  ["cardCallback", "card_callback", "接收卡片操作"],
] as const;

export function ConnectionCapabilities({
  record,
  channelNames,
}: {
  record: Connector;
  channelNames: Map<string, string>;
}) {
  const matrix = record.capabilities ?? {};
  const probes = record.capabilityProbes ?? [];
  return (
    <div className="space-y-5">
      <div>
        <Definition
          label="连接状态"
          value={<Status value={String(record.status ?? "unverified")} />}
        />
        <Definition
          label="最近检查"
          value={<Time value={record.verifiedAt} />}
        />
        {record.lastError ? (
          <p className="mt-3 rounded-lg bg-red-50 p-3 text-xs leading-6 text-red-800">
            {String(record.lastError)}
          </p>
        ) : null}
      </div>
      <div className="divide-y divide-border rounded-xl border border-border px-4">
        {capabilities.map(([key, , title]) => {
          const entry = matrix[key];
          const ids = entry?.channelIds ?? [];
          return (
            <div className="py-4" key={key}>
              <div className="flex items-center justify-between gap-3">
                <span className="text-xs font-medium">{title}</span>
                <Status value={entry?.status ?? "unverified"} />
              </div>
              {entry?.reason && <p className="mt-2 text-xs leading-6 text-muted-foreground">
                {entry.reason}
              </p>}
              {ids.length > 0 && (
                <p className="mt-1 break-all text-xs leading-6">
                  已确认渠道：
                  {ids
                    .map((id) => channelNames.get(String(id)) || String(id))
                    .join("、")}
                </p>
              )}
              {entry?.verifiedAt ? (
                <div className="mt-1">
                  <Time value={entry.verifiedAt} />
                </div>
              ) : null}
            </div>
          );
        })}
      </div>
      <div>
        <h3 className="mb-3 text-sm font-medium">各渠道检查结果</h3>
        {probes.length ? (
          <div className="space-y-3">
            {probes.map((probe, index) => {
              return (
                <div
                  key={index}
                  className="rounded-lg border border-border p-3 text-xs"
                >
                  <div className="flex items-center justify-between gap-3">
                    <span>
                      {capabilities.find(
                        ([, key]) => key === probe.capability,
                      )?.[2] ||
                        probe.capability ||
                        "功能检查"}
                    </span>
                    <Status value={String(probe.status ?? "unverified")} />
                  </div>
                  <p className="mt-2 break-all leading-6">
                    {channelNames.get(probe.channelId ?? "") ||
                      probe.externalId ||
                      probe.channelId ||
                      "未关联渠道"}
                  </p>
                  {probe.reason && <p className="mt-1 leading-6 text-muted-foreground">
                    {String(probe.reason)}
                  </p>}
                  <Time value={probe.checkedAt} />
                </div>
              );
            })}
          </div>
        ) : (
          <p className="text-xs leading-6 text-muted-foreground">
            暂无检查结果
          </p>
        )}
      </div>
    </div>
  );
}
