import test from "node:test";
import assert from "node:assert/strict";
import { auditRecordCategory, caseEventCategory, caseSourceLabel, externalOperationCategory } from "../lib/activity-labels.ts";

test("case sources distinguish manual intake from approval mode and platform names", () => {
  assert.equal(caseSourceLabel("manual"), "手动创建");
  assert.equal(caseSourceLabel("feishu"), "飞书");
  assert.equal(caseSourceLabel("lark"), "Lark");
  assert.equal(caseSourceLabel("github"), "GitHub");
});

test("case event categories expose record markers", () => {
  assert.deepEqual(caseEventCategory("feedback", "external:user"), { label: "用户反馈", tone: "feedback" });
  assert.deepEqual(caseEventCategory("reply_sent", "system:worker"), { label: "系统状态", tone: "system" });
  assert.deepEqual(caseEventCategory("source_updated", "external:user"), { label: "用户消息", tone: "user" });
});

test("notification delivery failures show as platform status", () => {
  assert.deepEqual(caseEventCategory("notification_failed", "system:worker"), { label: "系统状态", tone: "system" });
  assert.deepEqual(caseEventCategory("notification_outcome_unknown", "system:worker"), { label: "系统状态", tone: "system" });
});

test("audit categories distinguish feedback, approvals and platform operations", () => {
  assert.deepEqual(auditRecordCategory("feedback_resolved", "cases", "ou_user"), { label: "用户反馈", tone: "feedback" });
  assert.deepEqual(auditRecordCategory("auto_approve", "approvals", "system:policy:1"), { label: "自动审批", tone: "system" });
  assert.deepEqual(auditRecordCategory("external_succeeded", "external_operations", "system:worker"), { label: "平台操作", tone: "platform" });
  assert.deepEqual(auditRecordCategory("save", "cases", "u1"), { label: "操作", tone: "operation" });
  assert.deepEqual(externalOperationCategory(), { label: "平台操作", tone: "platform" });
});

