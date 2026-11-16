package taskcore

import "testing"

func TestTaskFences(t *testing.T) {
	s := State{"running", "edit", 2, 4}
	paused, err := Decide(s, Pause, Facts{})
	if err != nil || paused.Epoch != 5 {
		t.Fatal(paused, err)
	}
	if _, err = Decide(paused, Advance, Facts{Revision: 2, Epoch: 4, NextStage: "test"}); err != ErrStale {
		t.Fatal(err)
	}
	if _, err = Decide(paused, Resume, Facts{Unsettled: true}); err != ErrUnsettled {
		t.Fatal(err)
	}
	if _, err = Decide(s, Advance, Facts{Revision: 2, Epoch: 4, NextStage: "publish_pr"}); err != ErrState {
		t.Fatal(err)
	}
	if _, err = Decide(State{Status: "cancelled"}, Revise, Facts{}); err != ErrState {
		t.Fatal(err)
	}
	next, err := Decide(paused, Revise, Facts{})
	if err != nil || next.Stage != "plan" || next.Revision != 3 || next.Epoch != 6 {
		t.Fatal(next, err)
	}
}
func TestInputDigestOrderAndContent(t *testing.T) {
	a := []InputRef{{"b", "2"}, {"a", "1"}}
	b := []InputRef{{"a", "1"}, {"b", "2"}}
	if InputDigest(a) != InputDigest(b) {
		t.Fatal("order-dependent digest")
	}
	b[0].Digest = "changed"
	if InputDigest(a) == InputDigest(b) {
		t.Fatal("content not bound")
	}
	if a[0].ID != "b" {
		t.Fatal("mutated input")
	}
}

func TestTaskCreationAndTerminalFences(t *testing.T) {
	initial, e := Decide(State{}, Confirm, Facts{})
	if e != nil || initial.Status != "queued" || initial.Revision != 1 || initial.Epoch != 1 {
		t.Fatal(initial, e)
	}
	if _, e = Decide(initial, Confirm, Facts{}); e != ErrState {
		t.Fatal("duplicate creation", e)
	}
	for _, status := range []string{"cancelled", "completed"} {
		for _, command := range []Command{Pause, Cancel, Revoke, Advance, Block, Prepared, Started, Publishing, Published, Tracked, Retry} {
			if _, e = Decide(State{Status: status, Stage: "track", Revision: 1, Epoch: 1}, command, Facts{Revision: 1, Epoch: 1, NextStage: "track", Status: "pr_open"}); e == nil {
				t.Fatalf("%s accepted %s", status, command)
			}
		}
	}
}
