import test from "node:test";
import assert from "node:assert/strict";
import {
  changeResourceLimits,
  executionFields,
  executionFormValues,
  executionSettingsPayload,
  executionRuntimeIssue,
  modelFormValues,
  modelSettingsPayload,
  runtimeHostKnown,
  cgroupRuntimeLabel,
} from "../lib/settings-runtime.ts";

test("cgroup display distinguishes a disabled driver from missing runtime information", () => {
  assert.equal(cgroupRuntimeLabel({ cgroupVersion: "1", cgroupDriver: "none" }), "cgroup v1 · 未启用资源控制驱动");
  assert.equal(cgroupRuntimeLabel({ cgroupVersion: "2", cgroupDriver: "systemd" }), "cgroup v2 · systemd 驱动");
  assert.equal(cgroupRuntimeLabel({ cgroupVersion: "2", cgroupDriver: "cgroupfs" }), "cgroup v2 · cgroupfs 驱动");
  assert.equal(cgroupRuntimeLabel({ cgroupVersion: "2" }), "cgroup v2 · 驱动信息未提供");
  assert.equal(cgroupRuntimeLabel(), "资源限制状态未知");
});

const execution = {
  version: 4,
  resourceLimits: "required",
  networkMode: "none",
  readOnlyRootfs: true,
  testWorkspaceWritable: false,
  maxConcurrent: 3,
  timeoutSeconds: 900,
  memoryMb: 1024,
  cpuMillis: 1000,
  pids: 128,
  workspaceMb: 128,
  maxFiles: 20000,
  maxToolCalls: 40,
  maxTokens: 32000,
  artifactMb: 64,
  jobRetryAttempts: 5,
  jobRetryBaseSeconds: 2,
  jobRetryMaxSeconds: 64,
  externalRetryAttempts: 0,
  externalRetryDelaySeconds: 60,
};
const model = {
  version: 7,
  enabled: true,
  baseUrl: "https://models.example/v1",
  modelName: "test-model",
  apiKeyConfigured: true,
  jsonMode: "json_schema",
  maxTokensField: "max_tokens",
  maxDailyTokens: 1000000,
};

test("execution form submits numeric units unchanged and binds the loaded version", () => {
  const body = executionSettingsPayload(executionFormValues(execution), execution.version);
  const { version, ...settings } = execution;
  assert.deepEqual(body, { ...settings, expectedVersion: version });
  assert.equal(body.memoryMb, 1024);
  assert.equal(body.cpuMillis, 1000);
  assert.ok(!Object.hasOwn(body, "runtime"));
});

test("disabling cgroup quotas forces single concurrency while preserving quota values", () => {
  const changed = changeResourceLimits(executionFormValues(execution), "disabled");
  assert.equal(changed.maxConcurrent, "1");
  assert.equal(changed.memoryMb, "1024");
  assert.equal(changed.cpuMillis, "1000");
  assert.equal(changed.pids, "128");
  const body = executionSettingsPayload(changed, 4);
  assert.equal(body.resourceLimits, "disabled");
  assert.equal(body.maxConcurrent, 1);
  assert.throws(() => executionSettingsPayload({ ...changed, maxConcurrent: "2" }, 4));
  assert.equal(changeResourceLimits(changed, "required").memoryMb, "1024");
});

test("execution validation rejects out-of-range, empty and noninteger values", () => {
  for (const { key, min, max } of executionFields) {
    const values = executionFormValues(execution);
    for (const invalid of ["", " ", "1.5", "NaN", "Infinity", "1e3", String(min - 1), ...(max === undefined ? [] : [String(max + 1)])]) {
      assert.throws(() => executionSettingsPayload({ ...values, [key]: invalid }, 4), `${key}: ${invalid}`);
    }
    assert.doesNotThrow(() => executionSettingsPayload({ ...values, [key]: String(min) }, 4));
    if (max !== undefined) assert.doesNotThrow(() => executionSettingsPayload({ ...values, [key]: String(max) }, 4));
  }
  assert.throws(() => executionSettingsPayload({ ...executionFormValues(execution), resourceLimits: "unknown" }, 4));
});

test("file and model task quotas accept zero, finite values and large safe values", () => {
  const quotas = { workspaceMb: 1024, maxFiles: 100000, artifactMb: 2048, maxToolCalls: 1000, maxTokens: 1000000 };
  for (const [key, larger] of Object.entries(quotas)) {
    const field = executionFields.find((entry) => entry.key === key);
    assert.equal(field.min, 0);
    assert.ok(!Object.hasOwn(field, "max"), `${key} must not impose an HTML maximum`);
    for (const value of [0, 1, larger]) {
      const settings = { ...execution, [key]: value };
      const form = executionFormValues(settings);
      assert.equal(form[key], String(value));
      assert.equal(executionSettingsPayload(form, settings.version)[key], value);
    }
    assert.equal(executionFormValues({ ...execution, [key]: undefined })[key], "0");
  }
});

test("task quota numeric boundaries reject precision loss and MiB conversion overflow", () => {
  const limits = { workspaceMb: 8796093022207, artifactMb: 8796093022207, maxFiles: Number.MAX_SAFE_INTEGER, maxToolCalls: Number.MAX_SAFE_INTEGER, maxTokens: Number.MAX_SAFE_INTEGER };
  const values = executionFormValues(execution);
  for (const [key, max] of Object.entries(limits)) {
    assert.equal(executionSettingsPayload({ ...values, [key]: String(max) }, 4)[key], max);
    for (const invalid of [String(max + 1), "9007199254740993", "9".repeat(400)])
      assert.throws(() => executionSettingsPayload({ ...values, [key]: invalid }, 4), `${key}: ${invalid}`);
  }
});

test("execution controls preserve writable settings and omit the retired approval override", () => {
  const settings = { ...execution, networkMode: "bridge", readOnlyRootfs: false, testWorkspaceWritable: true, approvalMode: "auto", timeoutSeconds: 0 };
  const body = executionSettingsPayload(executionFormValues(settings), settings.version);
  assert.equal(body.networkMode, "bridge");
  assert.equal(body.readOnlyRootfs, false);
  assert.equal(body.testWorkspaceWritable, true);
  assert.ok(!Object.hasOwn(body, "approvalMode"));
  assert.equal(body.timeoutSeconds, 0);
  const defaults = executionFormValues({ ...execution, networkMode: undefined, readOnlyRootfs: undefined, testWorkspaceWritable: undefined, approvalMode: undefined });
  assert.equal(defaults.networkMode, "none");
  assert.equal(defaults.readOnlyRootfs, true);
  assert.equal(defaults.testWorkspaceWritable, false);
  assert.ok(!Object.hasOwn(defaults, "approvalMode"));
  assert.ok(!Object.hasOwn(executionSettingsPayload({ ...defaults, approvalMode: "manual" }, 4), "approvalMode"));
  for (const invalid of [{ networkMode: "host" }, { readOnlyRootfs: "false" }, { testWorkspaceWritable: "true" }])
    assert.throws(() => executionSettingsPayload({ ...defaults, ...invalid }, 4));
});

test("blank model key is omitted, explicit replacement and clearing are distinct", () => {
  const values = modelFormValues({ ...model, apiKey: "must-never-be-prefilled" });
  assert.equal(values.apiKey, "");
  assert.equal(values.clearApiKey, false);
  const body = modelSettingsPayload(values, model.version);
  assert.ok(!Object.hasOwn(body, "apiKey"));
  assert.ok(!Object.hasOwn(body, "apiKeyConfigured"));
  assert.equal(body.expectedVersion, 7);
  assert.equal(modelSettingsPayload({ ...values, apiKey: " new-secret " }, 7).apiKey, "new-secret");
  assert.ok(!Object.hasOwn(modelSettingsPayload({ ...values, apiKey: "   " }, 7), "apiKey"));
  const clear = modelSettingsPayload({ ...values, clearApiKey: true }, 7);
  assert.equal(clear.clearApiKey, true);
  assert.ok(!Object.hasOwn(clear, "apiKey"));
  assert.throws(() => modelSettingsPayload({ ...values, apiKey: "new-secret", clearApiKey: true }, 7));
});

test("enabled models require coordinates and HTTP must be explicitly allowed", () => {
  const values = modelFormValues(model);
  for (const baseUrl of ["", "not-a-url", "file:///tmp/model", "https://user:secret@models.example/v1", "https://models.example/v1?", "https://models.example/v1?token=secret", "https://models.example/v1#token"]) {
    assert.throws(() => modelSettingsPayload({ ...values, baseUrl }, 7), baseUrl);
  }
  assert.throws(() => modelSettingsPayload({ ...values, modelName: "  " }, 7));
  assert.throws(() => modelSettingsPayload({ ...values, baseUrl: "http://localhost:9000/v1" }, 7));
  assert.doesNotThrow(() => modelSettingsPayload({ ...values, baseUrl: "http://localhost:9000/v1", allowHttp: true }, 7));
  assert.doesNotThrow(() => modelSettingsPayload({ ...values, enabled: false, baseUrl: "", modelName: "" }, 7));
  for (const maxDailyTokens of ["", "0", "1000000001", "2.5"])
    assert.throws(() => modelSettingsPayload({ ...values, maxDailyTokens }, 7));
});

test("changing model destination requires replacing or explicitly clearing an existing key", () => {
  const values = { ...modelFormValues(model), baseUrl: "https://other.example/v1" };
  assert.throws(() => modelSettingsPayload(values, 7, model));
  assert.doesNotThrow(() => modelSettingsPayload({ ...values, apiKey: "replacement-key" }, 7, model));
  assert.doesNotThrow(() => modelSettingsPayload({ ...values, clearApiKey: true }, 7, model));
  assert.doesNotThrow(() => modelSettingsPayload({ ...values, baseUrl: `${model.baseUrl}/` }, 7, model));
  assert.doesNotThrow(() => modelSettingsPayload(values, 7, { ...model, apiKeyConfigured: false }));
});

test("unavailable container runtime leaves host capabilities unknown rather than unsupported", () => {
  assert.equal(runtimeHostKnown(undefined), false);
  assert.equal(runtimeHostKnown({ reachable: false }), false);
  const runtime = { reachable: true, rootless: false, supportsResourceLimits: false, errorEscaped: "runner runtime unavailable" };
  assert.equal(runtimeHostKnown(runtime), false);
  assert.match(executionRuntimeIssue(runtime).message, /容器环境检查未通过/);
  assert.equal(runtimeHostKnown({ reachable: true, cgroupVersion: "1", supportsResourceLimits: false }), true);
});

test("execution readiness uses Chinese actionable reasons and never echoes raw runtime errors", () => {
  assert.deepEqual(executionRuntimeIssue({ reachable: true, modelConfigured: false }), {
    message: "模型未就绪，请到“模型接口”启用并保存。", modelLink: true,
  });
  assert.match(executionRuntimeIssue({ reachable: true, resourceLimits: "required", supportsResourceLimits: false }).message, /不支持资源限制/);
  const issue = executionRuntimeIssue({ reachable: true, errorEscaped: "some new internal error" });
  assert.match(issue.message, /服务日志/);
  assert.ok(!issue.message.includes("internal"));
  assert.equal(executionRuntimeIssue({ reachable: true, ready: true }), undefined);
});
