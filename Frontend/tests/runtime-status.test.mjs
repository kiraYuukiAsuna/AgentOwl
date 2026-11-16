import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";
import { JsonParseNode } from "@microsoft/kiota-serialization-json";
import { createExecutionSettingsFromDiscriminatorValue } from "../lib/api/generated/models/index.ts";
import { runtimeHostKnown, executionRuntimeIssue, cgroupRuntimeLabel } from "../lib/settings-runtime.ts";

// Compile the actual status view; only layout primitives and icons are stubbed.
const source = ts.createSourceFile("runtime-settings.tsx", readFileSync(new URL("../components/runtime-settings.tsx", import.meta.url), "utf8"), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const functions = source.statements.filter(node => ts.isFunctionDeclaration(node) && ["ExecutionRuntime", "RuntimeMachineStatus"].includes(node.name?.text)).map(node => node.getText(source)).join("\n");
const compiled = ts.transpileModule(functions, { compilerOptions: { jsx: ts.JsxEmit.React, target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
const View = new Function("React", "Panel", "Button", "Link", "RefreshCw", "cn", "runtimeHostKnown", "executionRuntimeIssue", "cgroupRuntimeLabel", `${compiled}\nreturn ExecutionRuntime;`)(
  React,
  ({ title, action, children }) => React.createElement("section", null, React.createElement("h2", null, title), action, children),
  ({ children }) => React.createElement("button", null, children),
  ({ children, href }) => React.createElement("a", { href }, children),
  () => null,
  (...values) => values.filter(Boolean).join(" "), runtimeHostKnown, executionRuntimeIssue, cgroupRuntimeLabel,
);
const machine = {
  id: "machine-one", name: "Configured runner", reachable: true, ready: true,
  rootless: true, filesystem_ready: true, supports_resource_limits: false,
  settings_configured: true, model_configured: true, active_runs: 0,
  resource_limits: "disabled", max_concurrent: 1, cgroup_version: "1", cgroup_driver: "none",
  network_mode: "bridge", read_only_rootfs: true, test_workspace_writable: true,
};
function render(runtime) {
  const settings = new JsonParseNode({ runtime }).getObjectValue(createExecutionSettingsFromDiscriminatorValue);
  return renderToStaticMarkup(React.createElement(View, { settings, refresh() {} })).replace(/<[^>]+>/g, " ").replace(/\s+/g, " ");
}

test("configured pool renders machine details rather than missing summary fields", () => {
  const text = render({ source: "pool", reachable: true, ready: true, supports_resource_limits: false, machines: [machine] });
  for (const value of ["Configured runner", "Rootless 容器环境 已就绪", "运行盘与目录 已就绪", "执行配置 已同步", "模型接口 已启用", "正在运行 0 个任务", "并发上限 1 个任务", "网络 可联网", "根文件系统 只读", "测试目录 临时副本可写", "cgroup v1"])
    assert.ok(text.includes(value), `missing ${value}: ${text}`);
  assert.ok(!text.includes("待确认") && !text.includes("— 个任务"), text);
});

test("each machine retains its own readiness and unavailable host state", () => {
  const text = render({ source: "pool", reachable: true, ready: true, machines: [machine, { id: "machine-two", name: "Offline runner", reachable: false, ready: false }] });
  assert.match(text, /Configured runner 已就绪/);
  assert.match(text, /Offline runner 未连接/);
  assert.equal((text.match(/Rootless 容器环境/g) ?? []).length, 1);
});

test("empty machine pool retains the management entry without invented capabilities", () => {
  const text = render({ source: "pool", reachable: false, ready: false, machines: [] });
  assert.ok(text.includes("管理运行机器"));
  assert.ok(!text.includes("已就绪") && !text.includes("不支持"));
});

test("pool registration is not evidence that every host capability is known", () => {
  assert.equal(runtimeHostKnown({ source: "pool", reachable: true, machines: [{ reachable: false }] }), false);
  assert.equal(runtimeHostKnown({ source: "pool", reachable: true, machines: [{ reachable: true, rootless: true }] }), true);
});
