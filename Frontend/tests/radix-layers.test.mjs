import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const project = createRequire(import.meta.url);
const radix = createRequire(project.resolve("radix-ui"));
const command = createRequire(project.resolve("cmdk"));
const consumers = [
  createRequire(radix.resolve("@radix-ui/react-dialog")),
  createRequire(radix.resolve("@radix-ui/react-popover")),
  createRequire(command.resolve("@radix-ui/react-dialog")),
];

test("dialogs, searchable selects and the command palette share Radix interaction state", () => {
  // Separate module instances split the focus stack and let clicks pass through nested popovers.
  for (const dependency of ["@radix-ui/react-dismissable-layer", "@radix-ui/react-focus-scope"]) {
    const paths = consumers.map((consumer) => consumer.resolve(dependency));
    assert.equal(new Set(paths).size, 1, `${dependency} has separate instances:\n${paths.join("\n")}`);
  }
});
