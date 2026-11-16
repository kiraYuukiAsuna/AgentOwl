import test from "node:test";
import assert from "node:assert/strict";
import {
  changeSettingValue,
  settingFields,
  settingPayload,
  settingsSections,
} from "../lib/settings.ts";

const connectors = [
  { id: "feishu-1", name: "团队飞书", provider: "feishu" },
  { id: "lark-1", name: "团队 Lark", provider: "lark" },
  { id: "github-1", name: "Auth 仓库", provider: "github" },
];

const fieldKeys = (section, values, choices = connectors) =>
  new Set(settingFields(section, values, choices).map((field) => field.key));

function expectFields(keys, included, excluded) {
  for (const key of included) assert.ok(keys.has(key), `${key} should be shown`);
  for (const key of excluded)
    assert.ok(!keys.has(key), `${key} should be hidden`);
}

function expectCleared(values, keys) {
  for (const key of keys)
    assert.ok(values[key] === "" || values[key] === undefined, `${key} retained its old value`);
}

test("additional roles use named choices and preserve selected permissions in the payload", () => {
  const fields = settingFields("team", {});
  const roles = fields.find((field) => field.key === "roles");
  assert.equal(roles.kind, "lines");
  assert.deepEqual(roles.options, fields.find((field) => field.key === "role").options);
  assert.deepEqual(roles.options.map((option) => option.label), ["只读成员", "值班人员", "开发者", "知识库维护者", "管理员"]);
  const member = { name: "Test", email: "test@example.com", role: "viewer" };
  for (const selected of [[], ["admin"], ["developer", "knowledge"], roles.options.map((option) => option.value)]) {
    const body = settingPayload("team", { ...member, roles: selected });
    assert.deepEqual(body.roles, selected);
    assert.equal(body.role, "viewer");
  }
});

test("default Feishu and Lark forms show only their own platform fields", () => {
  const githubFields = ["owner", "repo", "repositoryId", "senderLogin", "webhookSecret", "token"];
  for (const values of [{}, { provider: "feishu" }, { provider: "lark" }]) {
    expectFields(
      fieldKeys("connections", values),
      ["name", "provider", "appId", "tenantId", "appSecret", "verificationToken", "encryptKey"],
      githubFields,
    );
  }
  assert.equal(settingsSections.connections.fields.find((field) => field.key === "provider")?.default, "feishu");
});

test("GitHub form hides Lark credentials and keeps optional settings advanced", () => {
  const fields = settingFields("connections", { provider: "github" });
  expectFields(
    new Set(fields.map((field) => field.key)),
    ["name", "provider", "owner", "repo", "repositoryId", "token", "webhookSecret"],
    ["appId", "tenantId", "appSecret", "verificationToken", "encryptKey"],
  );
  for (const key of ["baseUrl", "status", "senderLogin"])
    assert.equal(fields.find((field) => field.key === key)?.advanced, true, `${key} should be advanced`);
  for (const key of ["name", "provider"])
    assert.notEqual(fields.find((field) => field.key === key)?.advanced, true);
});

test("payload excludes hidden platform fields and empty edit passwords", () => {
  const body = settingPayload("connections", {
    name: "Team bot",
    provider: "feishu",
    appId: "cli_example",
    tenantId: "tenant_example",
    appSecret: "",
    verificationToken: "",
    encryptKey: "",
    owner: "other-platform",
    repo: "private-repo",
    repositoryId: "repository-old",
    senderLogin: "old-sender",
    token: "hidden-token-must-not-be-sent",
    webhookSecret: "hidden-webhook-secret",
    capabilities: { send: { status: "verified" } },
  });
  assert.equal(body.appId, "cli_example");
  for (const key of ["appSecret", "verificationToken", "encryptKey", "owner", "repo", "repositoryId", "senderLogin", "token", "webhookSecret", "capabilities"])
    assert.ok(!Object.hasOwn(body, key), `${key} must not be submitted`);

  const github = settingPayload("connections", {
    name: "Repository",
    provider: "github",
    owner: "team",
    repo: "auth",
    repositoryId: "",
    token: " keep-secret-whitespace ",
    webhookSecret: "",
    appSecret: "hidden-app-secret",
    appId: "hidden-app-id",
  });
  assert.equal(github.token, " keep-secret-whitespace ");
  assert.ok(!Object.hasOwn(github, "webhookSecret"));
  assert.ok(!Object.hasOwn(github, "appSecret"));
  assert.ok(!Object.hasOwn(github, "appId"));
});

test("switching platform clears the previous identity, secrets and API address", () => {
  const original = {
    name: "Support",
    provider: "feishu",
    status: "disabled",
    baseUrl: "https://old-api.example",
    appId: "cli_old",
    tenantId: "tenant_old",
    appSecret: "old-app-secret",
    verificationToken: "old-verification-token",
    encryptKey: "old-encryption-key",
  };
  const github = changeSettingValue("connections", original, "provider", "github");
  assert.equal(github.name, original.name);
  assert.equal(github.status, original.status);
  assert.equal(github.provider, "github");
  expectCleared(github, ["baseUrl", "appId", "tenantId", "appSecret", "verificationToken", "encryptKey"]);

  const lark = changeSettingValue("connections", {
    ...github,
    baseUrl: "https://github-api.example",
    owner: "team",
    repo: "auth",
    repositoryId: "repository-old",
    senderLogin: "old-account",
    token: "old-token",
    webhookSecret: "old-webhook-secret",
  }, "provider", "lark");
  assert.equal(lark.name, original.name);
  assert.equal(lark.status, original.status);
  assert.equal(lark.provider, "lark");
  expectCleared(lark, ["baseUrl", "owner", "repo", "repositoryId", "senderLogin", "token", "webhookSecret"]);
});

test("identity binding platform fields stay scoped to Feishu and Lark", () => {
  for (const values of [{}, { provider: "feishu" }, { provider: "lark" }]) {
    expectFields(
      fieldKeys("identities", values),
      ["principalId", "provider", "appId", "subjectId"],
      [],
    );
  }
  const provider = settingsSections.identities.fields.find((field) => field.key === "provider");
  assert.deepEqual(provider?.options?.map((option) => option.value), ["feishu", "lark"]);
  assert.throws(() => settingPayload("identities", { principalId: "principal-1", provider: "feishu", subjectId: "ou_user" }), /请填写应用 ID/);
  assert.throws(() => settingPayload("identities", { principalId: "principal-1", provider: "lark", appId: "cli_test" }), /请填写平台账号标识/);
});

test("identity binding provider switch clears stale identifiers", () => {
  const switched = changeSettingValue("identities", {
    principalId: "principal-1",
    provider: "feishu",
    appId: "cli_old",
    subjectId: "ou_old",
  }, "provider", "lark");
  assert.equal(switched.principalId, "principal-1");
  assert.equal(switched.provider, "lark");
  expectCleared(switched, ["appId", "subjectId"]);
});

test("linked GitHub repository hides manual coordinates and explicit unlink restores them", () => {
  const linked = {
    name: "Issues",
    provider: "github",
    repositoryId: "repository-1",
    owner: "stale-owner",
    repo: "stale-repository",
    token: "",
  };
  expectFields(fieldKeys("connections", linked), ["repositoryId"], ["owner", "repo"]);
  const linkedBody = settingPayload("connections", linked);
  assert.equal(linkedBody.repositoryId, "repository-1");
  assert.ok(!Object.hasOwn(linkedBody, "owner"));
  assert.ok(!Object.hasOwn(linkedBody, "repo"));
  assert.ok(!Object.hasOwn(linkedBody, "token"));

  const manual = {
    ...changeSettingValue("connections", linked, "repositoryId", ""),
    owner: "team",
    repo: "auth",
  };
  expectFields(fieldKeys("connections", manual), ["owner", "repo", "repositoryId"], []);
  const body = settingPayload("connections", manual);
  assert.ok(Object.hasOwn(body, "repositoryId"), "omitting repositoryId would retain the old server-side association");
  assert.equal(body.repositoryId, "");
  assert.equal(body.owner, "team");
  assert.equal(body.repo, "auth");
});

test("channel type follows the selected connector instead of stale form values", () => {
  for (const [connectorId, kind, externalId] of [
    ["feishu-1", "chat", "oc_group"],
    ["lark-1", "chat", "oc_lark_group"],
    ["github-1", "issue", "issue:12"],
  ]) {
    const body = settingPayload("channels", {
      name: "Support channel",
      connectorId,
      externalId,
      kind: kind === "chat" ? "issue" : "chat",
      replyMode: "draft",
      enabled: true,
    }, connectors);
    assert.equal(body.kind, kind);
    assert.equal(body.externalId, externalId);
    assert.equal(body.connectorId, connectorId);
  }
});

test("channel submission rejects absent, missing and unsupported connectors", () => {
  for (const connectorId of ["", "missing", "unsupported"]) {
    assert.throws(() => settingPayload("channels", {
      name: "Invalid channel",
      connectorId,
      externalId: "oc_group",
    }, [...connectors, { id: "unsupported", provider: "unknown" }]));
  }
  assert.throws(() => settingPayload("channels", {
    name: "Unloaded connector",
    connectorId: "feishu-1",
    externalId: "oc_group",
  }));
});

test("GitHub channels cannot keep the Lark mentions-only reply mode", () => {
  const values = { name: "Issues", connectorId: "github-1", externalId: "issue:12", replyMode: "mentions" };
  const fields = settingFields("channels", values, connectors);
  assert.ok(!fields.some((field) => field.key === "kind"), "channel kind should be determined by its connector");
  assert.ok(!fields.find((field) => field.key === "replyMode")?.options?.some((option) => option.value === "mentions"));
  assert.throws(() => settingPayload("channels", values, connectors));
  const larkFields = settingFields("channels", { ...values, connectorId: "feishu-1" }, connectors);
  assert.ok(larkFields.find((field) => field.key === "replyMode")?.options?.some((option) => option.value === "mentions"));
});

test("changing channel connector clears the old destination and mentions-only mode", () => {
  const changed = changeSettingValue("channels", {
    name: "Support channel",
    connectorId: "feishu-1",
    externalId: "oc_previous_group",
    replyMode: "mentions",
    visibility: "internal",
    enabled: true,
  }, "connectorId", "github-1");
  assert.equal(changed.connectorId, "github-1");
  expectCleared(changed, ["externalId"]);
  assert.equal(changed.replyMode, "draft");
  assert.equal(changed.name, "Support channel");
  assert.equal(changed.visibility, "internal");
  assert.equal(changed.enabled, true);
});

test("knowledge source types show and submit only applicable location fields", () => {
  for (const [kind, included, excluded] of [
    ["repository", ["repositoryId", "path"], ["connectorId", "documentId"]],
    ["lark", ["connectorId", "documentId", "url"], ["repositoryId", "path"]],
    ["manual", [], ["repositoryId", "path", "connectorId", "documentId"]],
  ]) {
    const values = {
      name: "Auth documentation",
      kind,
      repositoryId: "repository-1",
      path: "docs/auth.md",
      connectorId: "feishu-1",
      documentId: "doc-example",
      url: "https://example.feishu.cn/docx/doc-example",
      channelIds: ["channel-1"],
      visibility: "internal",
    };
    expectFields(fieldKeys("sources", values), included, excluded);
    const body = settingPayload("sources", values, connectors);
    for (const key of excluded)
      assert.ok(!Object.hasOwn(body, key), `${kind} source submitted irrelevant ${key}`);
    assert.equal(body.kind, kind);
    assert.deepEqual(body.channelIds, ["channel-1"]);
  }
});

test("changing source type clears old locations but retains audience settings", () => {
  const lark = changeSettingValue("sources", {
    name: "Auth documentation",
    kind: "repository",
    repositoryId: "repository-old",
    path: "docs/old.md",
    channelIds: ["channel-1"],
    visibility: "internal",
    status: "revoked",
  }, "kind", "lark");
  expectCleared(lark, ["repositoryId", "path"]);
  assert.equal(lark.kind, "lark");
  const manual = changeSettingValue("sources", {
    ...lark,
    connectorId: "feishu-1",
    documentId: "old-document",
  }, "kind", "manual");
  expectCleared(manual, ["connectorId", "documentId"]);
  assert.equal(manual.kind, "manual");
  assert.equal(manual.name, "Auth documentation");
  assert.equal(manual.status, "revoked");
  assert.equal(manual.visibility, "internal");
  assert.deepEqual(manual.channelIds, ["channel-1"]);
});

test("existing required fields remain required while incomplete credentials can be saved", () => {
  assert.doesNotThrow(() => settingPayload("connections", { name: "Bot draft", provider: "feishu" }));
  assert.doesNotThrow(() => settingPayload("connections", { name: "GitHub draft", provider: "github", repositoryId: "" }));
  assert.throws(() => settingPayload("connections", { name: "", provider: "feishu" }));
  assert.throws(() => settingPayload("connections", { name: "Missing platform", provider: "" }));
  assert.throws(() => settingPayload("channels", { name: "Group", connectorId: "feishu-1", externalId: "" }, connectors));
  assert.throws(() => settingPayload("repositories", { name: "Repository", owner: "team", repo: "" }));
});

test("multiline repository inputs trim entries and discard blank lines from textarea arrays or strings", () => {
  const paths = " internal/auth \n\n internal/token\n \t\n";
  const command = " go \n test \n\n ./internal/auth/... \n\t\n";
  for (const asArray of [true, false]) {
    const body = settingPayload("repositories", {
      name: "Auth repository",
      owner: "team",
      repo: "auth",
      allowedPaths: asArray ? paths.split("\n") : paths,
      testArgv: asArray ? command.split("\n") : command,
    });
    assert.deepEqual(body.allowedPaths, ["internal/auth", "internal/token"]);
    assert.deepEqual(body.testArgv, ["go", "test", "./internal/auth/..."]);
  }
});

test("polling interval is configurable for GitHub and Feishu channels", () => {
  const base = { name: "test", connectorId: "github-1", externalId: "team/repo", pollIntervalMinutes: "5" };
  assert.ok(fieldKeys("channels", base).has("pollIntervalMinutes"));
  assert.equal(settingPayload("channels", base, connectors).pollIntervalMinutes, 5);
  assert.equal(settingPayload("channels", { ...base, pollIntervalMinutes: "0" }, connectors).pollIntervalMinutes, 0);
  assert.throws(() => settingPayload("channels", { ...base, pollIntervalMinutes: "1441" }, connectors));
  assert.throws(() => settingPayload("channels", { ...base, pollIntervalMinutes: "1.5" }, connectors));
  assert.throws(() => settingPayload("channels", { ...base, externalId: "pull:2" }, connectors));
  assert.ok(fieldKeys("channels", { ...base, connectorId: "feishu-1" }).has("pollIntervalMinutes"));
  assert.equal(settingPayload("channels", { ...base, connectorId: "feishu-1", externalId: "oc_test" }, connectors).pollIntervalMinutes, 5);
});

test("long connection is configurable only for Feishu and Lark accounts", () => {
  for (const provider of ["feishu", "lark"]) {
    assert.ok(fieldKeys("connections", { provider }).has("wsEnabled"));
    assert.equal(settingPayload("connections", { provider, name: "bot", wsEnabled: true }).wsEnabled, true);
  }
  assert.ok(!fieldKeys("connections", { provider: "github" }).has("wsEnabled"));
  assert.ok(!Object.hasOwn(settingPayload("connections", { provider: "github", name: "repo", wsEnabled: true }), "wsEnabled"));
});

test("policy form exposes only fields used by the selected operation", () => {
  expectFields(
    fieldKeys("policies", { kind: "reply_send", mode: "auto" }),
    ["name", "kind", "mode", "channelIds", "maxFiles", "maxTokens", "maxSeconds", "expiresAt", "enabled"],
    ["repositoryIds", "allowedPaths", "requireTests"],
  );
  expectFields(
    fieldKeys("policies", { kind: "knowledge_publish", mode: "auto" }),
    ["name", "kind", "mode", "expiresAt", "enabled"],
    ["channelIds", "repositoryIds", "allowedPaths", "maxFiles", "maxTokens", "maxSeconds", "requireTests"],
  );
  expectFields(
    fieldKeys("policies", { kind: "edit", mode: "auto" }),
    ["repositoryIds", "allowedPaths", "maxFiles", "maxTokens", "maxSeconds"],
    ["channelIds", "requireTests"],
  );
  expectFields(
    fieldKeys("policies", { kind: "publish_pr", mode: "auto" }),
    ["repositoryIds", "allowedPaths"],
    ["channelIds", "requireTests"],
  );
});

test("policy payload clears stale scopes that would prevent matching", () => {
  const reply = settingPayload("policies", {
    name: "auto reply",
    kind: "reply_send",
    mode: "auto",
    channelIds: ["channel-1"],
    repositoryIds: ["repo-stale"],
    allowedPaths: ["auth"],
    requireTests: true,
  });
  assert.deepEqual(reply.channelIds, ["channel-1"]);
  assert.deepEqual(reply.repositoryIds, []);
  assert.deepEqual(reply.allowedPaths, []);
  assert.equal(reply.requireTests, false);
  assert.equal(reply.maxFiles, 1);

  const knowledge = settingPayload("policies", {
    name: "auto knowledge",
    kind: "knowledge_publish",
    mode: "auto",
    channelIds: ["channel-stale"],
    repositoryIds: ["repo-stale"],
    requireTests: true,
  });
  assert.deepEqual(knowledge.channelIds, []);
  assert.deepEqual(knowledge.repositoryIds, []);
  assert.equal(knowledge.requireTests, false);
  assert.equal(knowledge.maxTokens, 20000);
});

test("changing policy kind or mode clears no-longer-applicable draft fields", () => {
  const reply = changeSettingValue("policies", {
    name: "policy",
    kind: "edit",
    mode: "auto",
    repositoryIds: ["repo-old"],
    allowedPaths: ["auth"],
    requireTests: true,
  }, "kind", "reply_send");
  assert.equal(reply.kind, "reply_send");
  assert.ok(!Object.hasOwn(reply, "repositoryIds"));
  assert.ok(!Object.hasOwn(reply, "allowedPaths"));
  assert.ok(!Object.hasOwn(reply, "requireTests"));

  const manual = changeSettingValue("policies", {
    name: "policy",
    kind: "reply_send",
    mode: "auto",
    channelIds: ["channel-1"],
    maxFiles: 1,
    maxTokens: 2000,
    maxSeconds: 60,
  }, "mode", "manual");
  assert.deepEqual(manual.channelIds, ["channel-1"]);
  for (const key of ["maxFiles", "maxTokens", "maxSeconds"])
    assert.ok(!Object.hasOwn(manual, key), `${key} should be cleared`);
});


test("task creation shares policy modes with scoped checkbox fields and no execution budgets", () => {
  const values = {name:"Create fixes",kind:"create_task",mode:"auto",channelId:"channel",repositoryId:"repo",sponsorPrincipalId:"developer",condition:"reproducible defects",allowedPaths:["src"],senderTypes:["human","bot"]};
  expectFields(fieldKeys("policies",values), ["channelId","repositoryId","sponsorPrincipalId","condition","allowedPaths","senderTypes","followupMode"], ["channelIds","repositoryIds","maxFiles","maxTokens","maxSeconds","requireTests"]);
  const payload = settingPayload("policies",values);
  assert.deepEqual(payload.allowedPaths,["src"]);
  assert.equal(payload.allowBot,true);
  assert.ok(!Object.hasOwn(payload,"maxTokens"));
  assert.throws(() => settingPayload("policies",{...values,allowedPaths:[]}));
  assert.throws(() => settingPayload("policies",{...values,senderTypes:[]}));
  assert.deepEqual(changeSettingValue("policies",values,"repositoryId","other").allowedPaths,[]);
  const switched = changeSettingValue("policies",values,"kind","publish_pr");
  assert.ok(!Object.hasOwn(switched,"sponsorPrincipalId"));
  assert.ok(!Object.hasOwn(switched,"condition"));
});

test("policy expiration converts local date selection into a precise server timestamp", () => {
  const payload = settingPayload("policies",{name:"temporary",kind:"plan",mode:"manual",expiresAt:"2030-01-01T12:30"});
  assert.equal(payload.expiresAt,new Date("2030-01-01T12:30").toISOString());
  assert.throws(() => settingPayload("policies",{name:"bad",kind:"plan",mode:"manual",expiresAt:"bad date"}));
});


test("automatic policies reject zero limits instead of silently replacing them", () => {
  assert.throws(() => settingPayload("policies",{name:"auto",kind:"plan",mode:"auto",maxTokens:0}));
});


test("deny task creation only asks for the scope it enforces", () => {
  const values={name:"No tasks",kind:"create_task",mode:"deny",channelId:"channel",repositoryId:"repo"};
  expectFields(fieldKeys("policies",values),["channelId","repositoryId"],["condition","sponsorPrincipalId","allowedPaths","senderTypes","followupMode","maxTokens"]);
  assert.doesNotThrow(()=>settingPayload("policies",values));
});


test("every approval policy has a notification switch and optional internal channel", () => {
  for (const kind of ["create_task","reply_send","knowledge_publish","plan","edit","test","review","publish_pr"]) {
    for (const mode of ["manual","auto","deny"]) {
      const keys=fieldKeys("policies",{kind,mode});
      assert.ok(keys.has("notificationEnabled"));assert.ok(keys.has("notificationChannelId"));
      assert.ok(!fieldKeys("policies",{kind,mode,notificationEnabled:false}).has("notificationChannelId"));
    }
  }
  const base={name:"knowledge",kind:"knowledge_publish",mode:"manual"};
  assert.equal(settingPayload("policies",base).notificationEnabled,true);
  assert.equal(settingPayload("policies",{...base,notificationEnabled:false}).notificationEnabled,false);
  assert.equal(settingPayload("policies",{...base,notificationChannelId:"internal"}).notificationChannelId,"internal");
});
