package runner

import (
	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/taskcore"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskInputPages(t *testing.T) {
	body := strings.Repeat("中文abc", 180)
	inputs := []TaskInput{{ID: "one", Digest: "digest", Body: body}}
	cursor := ""
	got := ""
	for {
		p, e := TaskInputPage(inputs, "one", cursor)
		if e != nil {
			t.Fatal(e)
		}
		got += p.Text
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if got != body {
		t.Fatal("input truncated")
	}
	for _, bad := range []string{"-1", "1", "999999", "abc"} {
		if _, e := TaskInputPage(inputs, "one", bad); e == nil {
			t.Fatal(bad)
		}
	}
	if _, e := TaskInputPage(inputs, "other", ""); e != ErrNotFound {
		t.Fatal(e)
	}
}

func TestTaskInputReceiptsPersistOnlyExactPages(t *testing.T) {
	body := strings.Repeat("evidence ", 100)
	sum := sha256.Sum256([]byte(body))
	input := TaskInput{ID: "input-one", Digest: hex.EncodeToString(sum[:]), Body: body}
	spec := RunSpec{RunID: "receipt-test", TaskInputProtocol: 1, TaskInputs: []TaskInput{input}, TaskInputDigest: taskcore.InputDigest([]taskcore.InputRef{{ID: input.ID, Digest: input.Digest}})}
	if e := validateTaskInputs(spec); e != nil {
		t.Fatal(e)
	}
	legacy := RunSpec{}
	if e := validateTaskInputs(legacy); e != nil {
		t.Fatal(e)
	}
	altered := spec
	altered.TaskInputDigest = "different"
	if validateTaskInputs(altered) == nil {
		t.Fatal("accepted changed digest")
	}
	c := &Controller{cfg: Config{DataRoot: t.TempDir()}}
	if e := os.MkdirAll(c.runDir(spec.RunID), 0700); e != nil {
		t.Fatal(e)
	}
	a := &activeRun{spec: spec, run: Run{RunID: spec.RunID}}
	page, _ := TaskInputPage(spec.TaskInputs, input.ID, "")
	raw, _ := json.Marshal(page)
	bad := page
	bad.Text = "summary"
	wrong, _ := json.Marshal(bad)
	request := integration.ChatRequest{Messages: []integration.ChatMessage{{Role: "assistant", Content: string(raw)}, {Role: "tool", Content: string(wrong)}}}
	if e := c.recordIncludedInputs(a, request); e != nil {
		t.Fatal(e)
	}
	if len(a.run.Result.InputReceipts) != 0 {
		t.Fatal("summary or assistant text became input receipt")
	}
	request.Messages = append(request.Messages, integration.ChatMessage{Role: "tool", Content: string(raw)})
	for range 2 {
		if e := c.recordIncludedInputs(a, request); e != nil {
			t.Fatal(e)
		}
	}
	if len(a.run.Result.InputReceipts) != 1 {
		t.Fatal("receipt dedup failed")
	}
	stored, e := os.ReadFile(filepath.Join(c.runDir(spec.RunID), "run.json"))
	if e != nil {
		t.Fatal(e)
	}
	var recovered Run
	if e = json.Unmarshal(stored, &recovered); e != nil {
		t.Fatal(e)
	}
	if len(recovered.Result.InputReceipts) != 1 || recovered.Result.InputReceipts[0].PartID != "0" {
		t.Fatal("receipt not durable")
	}
}
