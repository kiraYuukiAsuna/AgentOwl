package integration

import (
	"encoding/json"
	"time"
)

// Event is source data, never an authorization decision or an executable command.
type Event struct {
	ID               string       `json:"id"`
	Kind             string       `json:"kind"`
	Action           string       `json:"action,omitempty"`
	AppID            string       `json:"app_id,omitempty"`
	TenantID         string       `json:"tenant_id,omitempty"`
	ChannelID        string       `json:"channel_id"`
	ChatType         string       `json:"chat_type,omitempty"`
	MessageID        string       `json:"message_id"`
	RootID           string       `json:"root_id,omitempty"`
	ParentID         string       `json:"parent_id,omitempty"`
	ThreadID         string       `json:"thread_id,omitempty"`
	ActorID          string       `json:"actor_id"`
	ActorTenantID    string       `json:"actor_tenant_id,omitempty"`
	SourceRevision   string       `json:"source_revision"`
	OccurredAt       time.Time    `json:"occurred_at"`
	Text             string       `json:"text"`
	MessageType      string       `json:"message_type,omitempty"`
	SourceURL        string       `json:"source_url,omitempty"`
	Repository       string       `json:"repository,omitempty"`
	Number           int          `json:"number,omitempty"`
	Bot              bool         `json:"bot"`
	Deleted          bool         `json:"deleted"`
	Attachments      []Attachment `json:"attachments,omitempty"`
	MentionedUserIDs []string     `json:"mentioned_user_ids,omitempty"`
}

type Attachment struct {
	Key  string `json:"key"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type InputImage struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

type ChatContentPart struct {
	Type  string      `json:"type"`
	Text  string      `json:"text,omitempty"`
	Image *InputImage `json:"image,omitempty"`
}

type ChatMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Parts      []ChatContentPart `json:"parts,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type JSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

type ChatRequest struct {
	Messages       []ChatMessage
	Tools          []ToolDefinition
	ResponseSchema *JSONSchema
	MaxTokens      int
}

type ChatResponse struct {
	ID           string
	Model        string
	Message      ChatMessage
	FinishReason string
	Usage        Usage
}
