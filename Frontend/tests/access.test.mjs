import test from "node:test";
import assert from "node:assert/strict";
import { hasRole, safeReturnPath } from "../lib/access.ts";
test("only assigned roles enable controls, including secondary roles", () => {
  assert.equal(hasRole(undefined, "developer"), false);
  assert.equal(hasRole({ role: "viewer" }, "developer"), false);
  assert.equal(
    hasRole({ role: "oncall", roles: ["knowledge"] }, "knowledge"),
    true,
  );
  assert.equal(hasRole({ role: "admin" }, "developer"), true);
});
test("login redirects stay on the application origin", () => {
  for (const value of [
    null,
    "https://evil.example",
    "//evil.example",
    "/\\evil.example",
    "javascript:alert(1)",
  ])
    assert.equal(safeReturnPath(value), "/");
  assert.equal(
    safeReturnPath("/cases/a?tab=reply#draft"),
    "/cases/a?tab=reply#draft",
  );
});
