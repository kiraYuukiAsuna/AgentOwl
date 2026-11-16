import test from "node:test";
import assert from "node:assert/strict";

const choices = [
  { action: "resolved", success: "已标记为已解决" },
  { action: "unresolved", success: "已标记为未解决" },
  { action: "human", success: "已转人工" },
];

function nextFeedbackState(state, event) {
  switch (event.type) {
    case "submit":
      return { ...state, busy: event.action };
    case "saved":
      return { ...state, busy: null, choice: null, reason: "" };
    case "close":
      return state.busy ? state : { ...state, choice: null, reason: "" };
    default:
      return state;
  }
}

test("feedback submit clears dialog after save", () => {
  let state = { choice: choices[0], busy: null, reason: "用户确认已恢复" };
  state = nextFeedbackState(state, { type: "submit", action: "resolved" });
  assert.equal(state.busy, "resolved");
  assert.equal(state.choice.action, "resolved");
  state = nextFeedbackState(state, { type: "saved" });
  assert.equal(state.busy, null);
  assert.equal(state.choice, null);
  assert.equal(state.reason, "");
});

test("feedback success copy stays specific per action", () => {
  assert.deepEqual(
    choices.map((item) => item.success),
    [
      "已标记为已解决",
      "已标记为未解决",
      "已转人工",
    ],
  );
});

test("busy feedback dialog ignores close", () => {
  let state = { choice: choices[0], busy: "resolved", reason: "用户确认已恢复" };
  state = nextFeedbackState(state, { type: "close" });
  assert.equal(state.choice.action, "resolved");
  assert.equal(state.reason, "用户确认已恢复");
});
