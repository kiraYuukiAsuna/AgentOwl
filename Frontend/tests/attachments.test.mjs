import test from "node:test";
import assert from "node:assert/strict";
import {
  attachmentFilename,
  attachmentReferences,
} from "../lib/attachments.ts";

test("attachment references preserve server indexes across invalid entries", () => {
  const items = attachmentReferences([
    null,
    { key: "file-key", type: "file", name: "auth.log" },
    { key: "unsafe-url", type: "url" },
  ]);
  assert.equal(items.length, 3);
  assert.equal(items[0].readable, false);
  assert.equal(items[1].index, 1);
  assert.equal(items[1].readable, true);
  assert.equal(items[2].readable, false);
  assert.deepEqual(attachmentReferences({ key: "file-key" }), []);
});

test("download filenames do not retain paths or control characters", () => {
  assert.equal(attachmentFilename("../auth\\log\n.txt", 0), ".._auth_log_.txt");
  assert.equal(attachmentFilename("", 2), "attachment-3.bin");
});

test("attachment downloads keep only safe filename suffixes", () => {
  for (const name of ["evil.html", "installer.exe", "unknown"]) {
    assert.equal(attachmentFilename(name, 0), `${name}.bin`);
  }
  assert.equal(attachmentFilename("report.PDF", 0), "report.PDF");
});
