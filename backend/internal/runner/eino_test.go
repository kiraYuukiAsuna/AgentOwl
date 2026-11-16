package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentowl/backend/internal/integration"
)

func einoToolReply(calls ...integration.ToolCall) integration.ChatResponse {
	return integration.ChatResponse{
		FinishReason: "tool_calls",
		Usage:        integration.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		Message:      integration.ChatMessage{Role: "assistant", ToolCalls: calls},
	}
}

func einoFinalReply(content string) integration.ChatResponse {
	return integration.ChatResponse{
		FinishReason: "stop",
		Usage:        integration.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		Message:      integration.ChatMessage{Role: "assistant", Content: content},
	}
}

func TestEinoMultipleToolsRunInOrderAtCallLimit(t *testing.T) {
	const changed = "package main\n// approved ordered change\n"
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, request integration.ChatRequest) (integration.ChatResponse, error) {
		switch calls.Add(1) {
		case 1:
			return einoToolReply(
				integration.ToolCall{ID: "read-before", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)},
				integration.ToolCall{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"package main\n// approved ordered change\n","executable":false}`)},
				integration.ToolCall{ID: "read-after", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)},
			), nil
		case 2:
			var replies []integration.ChatMessage
			for _, message := range request.Messages {
				if message.Role == "tool" {
					replies = append(replies, message)
				}
			}
			if len(replies) != 3 {
				t.Errorf("expected three ordered tool results, got %d", len(replies))
			} else {
				for i, id := range []string{"read-before", "write", "read-after"} {
					if replies[i].ToolCallID != id {
						t.Errorf("tool result %d belongs to %q, want %q", i, replies[i].ToolCallID, id)
					}
				}
				if replies[0].Content != "package main\n" || replies[2].Content != changed {
					t.Errorf("read/write/read order not preserved: before=%q after=%q", replies[0].Content, replies[2].Content)
				}
			}
		default:
			t.Error("unexpected extra model request")
		}
		return einoFinalReply("Applied the approved edit after reading the source. Tests not run."), nil
	})
	c, _, _ := fixture(t, model)
	spec := specFor("edit")
	spec.Limits.MaxToolCalls = 3
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, c, spec.RunID)
	if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.OutputDigest == spec.InputDigest || run.Result.ToolCalls != 3 || calls.Load() != 2 {
		t.Fatalf("ordered tool run failed: %+v; model requests=%d", run, calls.Load())
	}
}

func TestEinoRejectedToolConsumesCallBudget(t *testing.T) {
	for _, afterFatal := range []bool{false, true} {
		name := "write-exceeds-limit"
		if afterFatal {
			name = "write-after-fatal-overflow"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c, _, _ := fixture(t, modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
				calls.Add(1)
				batch := []integration.ToolCall{{ID: "missing", Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.go"}`)}}
				if afterFatal {
					batch = append(batch, integration.ToolCall{ID: "fatal-overflow", Name: "list_files", Arguments: json.RawMessage(`{"prefix":""}`)})
				}
				batch = append(batch, integration.ToolCall{ID: "must-not-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"unauthorized extra call","executable":false}`)})
				return einoToolReply(batch...), nil
			}))
			spec := specFor("edit")
			spec.Limits.MaxToolCalls = 1
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			if run.State != "failed" || !strings.Contains(run.Result.Error, "tool-call budget exhausted") || run.Result.EvidenceValid || run.Result.OutputDigest != spec.InputDigest || calls.Load() != 1 {
				t.Fatalf("refused read permitted an extra write or retry: %+v; requests=%d", run, calls.Load())
			}
			data, err := os.ReadFile(filepath.Join(c.runDir(spec.RunID), "tools.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 1 {
				t.Fatalf("expected exactly one attempted tool, got %q", data)
			}
			var entry struct {
				Tool    string `json:"tool"`
				Success bool   `json:"success"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil || entry.Tool != "read_file" || entry.Success {
				t.Fatalf("refused tool was not recorded: %s (%v)", lines[0], err)
			}
		})
	}
}

func TestEinoPermissionLossOnModelReturnBlocksToolsAndFinal(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "write"
		if final {
			name = "final-report"
		}
		t.Run(name, func(t *testing.T) {
			var c *Controller
			var auth *testAuthorizer
			var calls atomic.Int32
			spec := specFor("plan")
			if !final {
				spec = specFor("edit")
			}
			model := modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
				if calls.Add(1) == 1 {
					return einoToolReply(integration.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}), nil
				}
				// Expire the permit exactly as a model response becomes available.
				// Denial prevents the background renewal from restoring it.
				auth.deny.Store(true)
				c.mu.Lock()
				active := c.runs[spec.RunID]
				active.permissionLost = true
				active.permitUntil = time.Time{}
				c.mu.Unlock()
				if final {
					return einoFinalReply("Source inspected; proposed plan complete. Tests not run."), nil
				}
				return einoToolReply(integration.ToolCall{ID: "late-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"write after revocation","executable":false}`)}), nil
			})
			c, _, auth = fixture(t, model)
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			if run.State == "succeeded" || run.Result.EvidenceValid || run.Result.OutputDigest != spec.InputDigest || calls.Load() != 2 {
				t.Fatalf("expired permit accepted %s: %+v; requests=%d", name, run, calls.Load())
			}
		})
	}
}

func TestEinoFinalRequiresSuccessfulSourceRead(t *testing.T) {
	for _, beforeFinal := range []string{"none", "list", "failed-read"} {
		t.Run(beforeFinal, func(t *testing.T) {
			var calls atomic.Int32
			c, _, _ := fixture(t, modelFunc(func(context.Context, integration.ChatRequest) (integration.ChatResponse, error) {
				if calls.Add(1) == 1 {
					switch beforeFinal {
					case "list":
						return einoToolReply(integration.ToolCall{ID: "list", Name: "list_files", Arguments: json.RawMessage(`{"prefix":""}`)}), nil
					case "failed-read":
						return einoToolReply(integration.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.go"}`)}), nil
					}
				}
				return einoFinalReply("Claimed completion without inspecting a source file."), nil
			}))
			spec := specFor("plan")
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			wantCalls := int32(2)
			if beforeFinal == "none" {
				wantCalls = 1
			}
			if run.State == "succeeded" || run.Result.EvidenceValid || calls.Load() != wantCalls {
				t.Fatalf("uninspected source accepted or retried: %+v; requests=%d", run, calls.Load())
			}
		})
	}
}

func TestEinoRealModelClientPreservesProtocolAndUsage(t *testing.T) {
	for _, missingUsage := range []bool{false, true} {
		name := "review-round-trip"
		if missingUsage {
			name = "missing-usage-fails-with-reservation"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-model-key" {
					t.Errorf("unexpected model endpoint or authentication: %s %s", r.Method, r.URL.Path)
				}
				var request struct {
					MaxTokens int   `json:"max_tokens"`
					Stream    bool  `json:"stream"`
					Parallel  *bool `json:"parallel_tool_calls"`
					Messages  []struct {
						Role, Content string
						ToolCallID    string `json:"tool_call_id"`
						ToolCalls     []struct {
							ID       string                           `json:"id"`
							Function struct{ Name, Arguments string } `json:"function"`
						} `json:"tool_calls"`
					} `json:"messages"`
					Tools []struct {
						Function struct {
							Name       string
							Parameters struct {
								AdditionalProperties *bool    `json:"additionalProperties"`
								Required             []string `json:"required"`
								Properties           struct {
									Path struct {
										Type      string `json:"type"`
										MinLength int    `json:"minLength"`
										MaxLength int    `json:"maxLength"`
									} `json:"path"`
								} `json:"properties"`
							}
						}
					} `json:"tools"`
					ResponseFormat struct {
						Type   string
						Schema struct {
							Name   string
							Strict bool
							Schema json.RawMessage
						} `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if request.MaxTokens <= 0 || request.MaxTokens > 4096 || request.Stream || request.Parallel == nil || *request.Parallel {
					t.Errorf("model limits changed: max=%d stream=%v parallel=%v", request.MaxTokens, request.Stream, request.Parallel)
				}
				if request.ResponseFormat.Type != "json_schema" || request.ResponseFormat.Schema.Name != "agentowl_code_review" || !request.ResponseFormat.Schema.Strict || !json.Valid(request.ResponseFormat.Schema.Schema) {
					t.Error("structured review schema lost in model adapter")
				}
				seenRead := false
				seenContext := map[string]bool{}
				for _, tool := range request.Tools {
					switch tool.Function.Name {
					case "read_file", "list_files":
						if tool.Function.Name == "read_file" {
							seenRead = true
							parameters := tool.Function.Parameters
							if parameters.AdditionalProperties == nil || *parameters.AdditionalProperties || len(parameters.Required) != 1 || parameters.Required[0] != "path" || parameters.Properties.Path.Type != "string" || parameters.Properties.Path.MinLength != 1 || parameters.Properties.Path.MaxLength != 1024 {
								t.Error("read_file argument schema changed")
							}
						}
					case "read_context_manifest", "read_context_ref":
						seenContext[tool.Function.Name] = true
					default:
						t.Errorf("review received write or unknown tool %q", tool.Function.Name)
					}
				}
				if !seenRead {
					t.Error("read tool missing")
				}
				if !seenContext["read_context_manifest"] || !seenContext["read_context_ref"] {
					t.Error("context tools missing")
				}
				message := map[string]any{"role": "assistant", "content": ""}
				finish := "tool_calls"
				if n == 1 {
					message["tool_calls"] = []any{map[string]any{"id": "http-read-id", "type": "function", "function": map[string]string{"name": "read_file", "arguments": `{"path":"main.go"}`}}}
				} else {
					seenAssistant, seenResult := false, false
					for _, msg := range request.Messages {
						if msg.Role == "assistant" && len(msg.ToolCalls) == 1 && msg.ToolCalls[0].ID == "http-read-id" && msg.ToolCalls[0].Function.Name == "read_file" && msg.ToolCalls[0].Function.Arguments == `{"path":"main.go"}` {
							seenAssistant = true
						}
						if msg.Role == "tool" && msg.ToolCallID == "http-read-id" && msg.Content == "package main\n" {
							seenResult = true
						}
					}
					if n != 2 || !seenAssistant || !seenResult {
						t.Errorf("tool protocol lost or model retried: requests=%d assistant=%v result=%v", n, seenAssistant, seenResult)
					}
					message["content"] = `{"verdict":"pass","summary":"Inspected main.go. Tests not rerun.","findings":[]}`
					finish = "stop"
				}
				response := map[string]any{"id": "completion", "model": "test", "choices": []any{map[string]any{"message": message, "finish_reason": finish}}}
				if !missingUsage {
					response["usage"] = map[string]int{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150}
				}
				writeJSON(w, 200, response)
			}))
			defer server.Close()
			model, err := integration.NewModelClient(integration.ModelConfig{BaseURL: server.URL + "/v1", APIKey: "test-model-key", Model: "test", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			c, _, _ := fixture(t, model)
			spec := specFor("review")
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			if run.Result.Tokens <= 0 || spec.Limits.MaxTokens > 0 && run.Result.Tokens > int64(spec.Limits.MaxTokens) {
				t.Fatalf("model usage/reservation lost: %+v", run)
			}
			if missingUsage {
				if run.State == "succeeded" || run.Result.EvidenceValid || requests.Load() != 1 {
					t.Fatalf("missing usage treated as free or retried: %+v; requests=%d", run, requests.Load())
				}
			} else if run.State != "succeeded" || !run.Result.EvidenceValid || run.Result.ToolCalls != 1 || run.Result.Tokens < 300 || requests.Load() != 2 {
				t.Fatalf("model protocol round trip failed: %+v; requests=%d", run, requests.Load())
			}
		})
	}
}
