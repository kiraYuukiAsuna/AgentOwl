import test from "node:test";
import assert from "node:assert/strict";
import { approvalExecutionDetails, approvalTarget, formatTaskQuota } from "../lib/workflow.ts";

test("approval quota formatting distinguishes unlimited, finite and missing amounts", () => {
  assert.equal(formatTaskQuota(0, "Token"), "不限");
  assert.equal(formatTaskQuota(32000, "Token"), "32000 Token");
  assert.equal(formatTaskQuota(1000000, "Token"), "1000000 Token");
  assert.equal(formatTaskQuota(0, "个文件"), "不限");
  assert.equal(formatTaskQuota(undefined, "Token"), "未提供");
  assert.equal(formatTaskQuota(-1, "Token"), "未提供");
});

test("approval navigation uses parent IDs instead of reply and step IDs", () => {
  assert.equal(
    approvalTarget({
      subjectType: "reply",
      subjectId: "reply-1",
      action: { additionalData: { case_id: "case-1" } },
    }),
    "/cases/case-1",
  );
  assert.equal(
    approvalTarget({
      subjectType: "step",
      subjectId: "step-1",
      action: { additionalData: { task_id: "task-1" } },
    }),
    "/development/task-1",
  );
  assert.equal(
    approvalTarget({ subjectType: "step", subjectId: "step-1" }),
    undefined,
  );
  assert.equal(
    approvalTarget({ subjectType: "knowledge", subjectId: "knowledge-1" }),
    "/knowledge/knowledge-1",
  );
});

test("approval evidence shows frozen execution permissions and zero timeout explicitly", () => {
  const fields = Object.fromEntries(approvalExecutionDetails({
    timeout_seconds: 0,
    execution_profile: { network_mode: "bridge", read_only_rootfs: false, test_workspace_writable: true },
    approval_mode: "auto",
  }).map(({ label, value }) => [label, value]));
  assert.equal(fields["最长运行时间"], "不限时");
  assert.equal(fields["网络"], "可联网");
  assert.match(fields["根文件系统"], /可写/);
  assert.match(fields["测试目录"], /新增文件不进 PR/);
  assert.match(fields["审批方式"], /自动批准/);
});

test("approval evidence requires current execution fields", () => {
  const missing = Object.fromEntries(approvalExecutionDetails({ timeout_seconds: 60 }).map(({ label, value }) => [label, value]));
  assert.equal(missing["最长运行时间"], "60 秒");
  assert.equal(missing["网络"], "未提供");
  assert.equal(missing["根文件系统"], "未提供");
  assert.equal(missing["测试目录"], "未提供");
  assert.equal(missing["审批方式"], "未提供");
  assert.match(approvalExecutionDetails({ execution_profile: { network_mode: "host" } })[1].value, /未提供/);
});


test("task creation approval links back to its case", () => {
  assert.equal(approvalTarget({kind:"create_task",subjectType:"admission",subjectId:"decision",action:{additionalData:{case_id:"issue"}}}),"/cases/issue");
});
