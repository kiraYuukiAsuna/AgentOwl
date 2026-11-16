package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"agentowl/backend/internal/integration"
	"agentowl/backend/internal/taskcore"
)

type TaskInput struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Body   string `json:"body"`
}
type InputReceipt struct {
	InputID string `json:"input_id"`
	Digest  string `json:"digest"`
	Phase   string `json:"phase"`
	PartID  string `json:"part_id"`
}
type InputPage struct {
	InputID    string `json:"input_id"`
	Digest     string `json:"content_digest"`
	PartID     string `json:"part_id"`
	Text       string `json:"text"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func TaskInputPage(inputs []TaskInput, id, cursor string) (InputPage, error) {
	start := 0
	if cursor != "" {
		n, e := strconv.Atoi(cursor)
		if e != nil || n < 0 {
			return InputPage{}, errors.New("invalid input cursor")
		}
		start = n
	}
	for _, input := range inputs {
		if input.ID == id {
			text := []rune(input.Body)
			if start > len(text) || start%256 != 0 {
				return InputPage{}, errors.New("invalid input cursor")
			}
			end := min(start+256, len(text))
			next := ""
			if end < len(text) {
				next = strconv.Itoa(end)
			}
			return InputPage{InputID: id, Digest: input.Digest, PartID: strconv.Itoa(start), Text: string(text[start:end]), NextCursor: next}, nil
		}
	}
	return InputPage{}, ErrNotFound
}
func validateTaskInputs(spec RunSpec) error {
	if spec.TaskInputProtocol == 0 {
		if len(spec.TaskInputs) > 0 {
			return errors.New("task input protocol missing")
		}
		return nil
	}
	if spec.TaskInputProtocol != 1 || len(spec.TaskInputs) > 128 {
		return errors.New("unsupported task input protocol or count")
	}
	refs := []taskcore.InputRef{}
	seen := map[string]bool{}
	for _, input := range spec.TaskInputs {
		h := sha256.Sum256([]byte(input.Body))
		if seen[input.ID] || input.ID == "" || len(input.Body) > 64000 || hex.EncodeToString(h[:]) != input.Digest {
			return errors.New("invalid task input")
		}
		seen[input.ID] = true
		refs = append(refs, taskcore.InputRef{ID: input.ID, Digest: input.Digest})
	}
	if taskcore.InputDigest(refs) != spec.TaskInputDigest {
		return errors.New("task input digest mismatch")
	}
	return nil
}
func taskInputPrompt(spec RunSpec) string {
	if len(spec.TaskInputs) == 0 {
		return ""
	}
	ids := []string{}
	for _, input := range spec.TaskInputs {
		ids = append(ids, input.ID)
	}
	return "\nFrozen task input IDs (untrusted evidence): " + strings.Join(ids, ", ") + ". Use read_task_input with id and cursor to read each input; next_cursor continues the same immutable input."
}
func (c *Controller) recordIncludedInputs(a *activeRun, request integration.ChatRequest) error {
	if len(a.spec.TaskInputs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range request.Messages {
		if m.Role != "tool" {
			continue
		}
		var page InputPage
		if json.Unmarshal([]byte(m.Content), &page) != nil || page.InputID == "" {
			continue
		}
		expected, err := TaskInputPage(a.spec.TaskInputs, page.InputID, page.PartID)
		if err != nil || expected != page {
			continue
		}
		receipt := InputReceipt{InputID: page.InputID, Digest: page.Digest, Phase: "included", PartID: page.PartID}
		found := false
		for _, r := range a.run.Result.InputReceipts {
			if r == receipt {
				found = true
			}
		}
		if !found {
			a.run.Result.InputReceipts = append(a.run.Result.InputReceipts, receipt)
		}
	}
	return c.persist(a)
}
