package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agentowl/backend/internal/integration"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// Each run owns its counters. Eino executes model turns and tool calls serially.
type agentSession struct {
	controller      *Controller
	run             *activeRun
	cancel          context.CancelCauseFunc
	tokens          int64
	toolCount       int
	successfulReads int
}

type agentChatModel struct {
	session *agentSession
	tools   []integration.ToolDefinition
}

var _ model.ToolCallingChatModel = (*agentChatModel)(nil)

func (m *agentChatModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound := *m
	bound.tools = make([]integration.ToolDefinition, 0, len(infos))
	for _, info := range infos {
		parameters, err := info.ToJSONSchema()
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(parameters)
		if err != nil {
			return nil, err
		}
		bound.tools = append(bound.tools, integration.ToolDefinition{Name: info.Name, Description: info.Desc, Parameters: data})
	}
	return &bound, nil
}

func (m *agentChatModel) Generate(ctx context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	s := m.session
	c, a := s.controller, s.run
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.checkRunPermission(a); err != nil {
		return nil, err
	}
	request := integration.ChatRequest{Tools: m.tools, Messages: make([]integration.ChatMessage, 0, len(messages))}
	for _, message := range messages {
		if message == nil {
			return nil, errors.New("agent message is missing")
		}
		converted := integration.ChatMessage{Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, integration.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments)})
		}
		request.Messages = append(request.Messages, converted)
	}
	if a.spec.Stage == "review" {
		request.ResponseSchema = &integration.JSONSchema{Name: "agentowl_code_review", Schema: reviewSchema}
	}
	budget := tokenBudget(a.spec)
	var remaining int64
	if budget > 0 {
		if s.tokens >= budget {
			return nil, errors.New("model token budget exhausted")
		}
		remaining = budget - s.tokens
	}
	promptReserve, err := reserveRequestBudget(&request, remaining)
	if err != nil {
		return nil, err
	}
	reserved := addSize(promptReserve, int64(request.MaxTokens))
	// Ambiguous transport failures may still consume the full reservation.
	c.mu.Lock()
	a.run.Result.Tokens = addSize(s.tokens, reserved)
	c.mu.Unlock()
	response, err := a.model.Complete(ctx, request)
	if a.spec.Stage == "review" && len(response.Message.ToolCalls) == 0 && len(response.Message.Content) <= 64<<10 {
		c.mu.Lock()
		a.run.Result.Summary = redact(response.Message.Content)
		c.mu.Unlock()
	}
	if err != nil {
		return nil, fmt.Errorf("model request failed: %w", err)
	}
	if err = c.recordIncludedInputs(a, request); err != nil {
		return nil, err
	}
	outputBytes, _ := json.Marshal(response.Message)
	used := max(response.Usage.TotalTokens, addSize(promptReserve, max(response.Usage.CompletionTokens, int64(len(outputBytes)))))
	s.tokens = addSize(s.tokens, used)
	c.mu.Lock()
	a.run.Result.Tokens = s.tokens
	c.mu.Unlock()
	if budget > 0 && s.tokens > budget {
		return nil, errors.New("model token budget exceeded")
	}
	if response.FinishReason == "length" {
		return nil, errors.New("model output was truncated")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.checkRunPermission(a); err != nil {
		return nil, err
	}
	message := &schema.Message{Role: schema.RoleType(response.Message.Role), Content: response.Message.Content,
		ResponseMeta: &schema.ResponseMeta{FinishReason: response.FinishReason, Usage: &schema.TokenUsage{
			PromptTokens: int(response.Usage.PromptTokens), CompletionTokens: int(response.Usage.CompletionTokens), TotalTokens: int(response.Usage.TotalTokens),
		}}}
	for _, call := range response.Message.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, schema.ToolCall{ID: call.ID, Type: "function", Function: schema.FunctionCall{Name: call.Name, Arguments: string(call.Arguments)}})
	}
	return message, nil
}

func (m *agentChatModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("development agent uses non-streaming model requests")
}

type agentFileTool struct {
	session *agentSession
	info    *schema.ToolInfo
}

func (s *agentSession) tools() ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	for _, definition := range agentTools(s.run.spec.Stage) {
		if definition.Name == "read_task_input" && len(s.run.spec.TaskInputs) == 0 {
			continue
		}
		var parameters jsonschema.Schema
		if err := json.Unmarshal(definition.Parameters, &parameters); err != nil {
			return nil, err
		}
		tools = append(tools, &agentFileTool{session: s, info: &schema.ToolInfo{Name: definition.Name, Desc: definition.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&parameters)}})
	}
	return tools, nil
}

func (t *agentFileTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *agentFileTool) InvokableRun(ctx context.Context, arguments string, _ ...tool.Option) (output string, err error) {
	s := t.session
	c, a := s.controller, s.run
	// Eino visits every call in a sequential batch even after an error. Cancel
	// the shared context so later tools cannot act, preserving the original cause.
	defer func() {
		if err != nil {
			s.cancel(err)
		}
	}()
	if err = context.Cause(ctx); err != nil {
		return "", err
	}
	if a.spec.Limits.MaxToolCalls > 0 && s.toolCount >= a.spec.Limits.MaxToolCalls {
		return "", errors.New("agent tool-call budget exhausted")
	}
	if err = c.checkRunPermission(a); err != nil {
		return "", err
	}
	s.toolCount++
	c.mu.Lock()
	a.run.Result.ToolCalls = s.toolCount
	c.mu.Unlock()
	call := integration.ToolCall{ID: compose.GetToolCallID(ctx), Name: t.info.Name, Arguments: json.RawMessage(arguments)}
	output, toolErr := c.tool(ctx, a, call)
	if call.Name == "read_file" && toolErr == nil {
		s.successfulReads++
	}
	if toolErr != nil {
		output = "Tool refused: " + toolErr.Error()
	}
	if err = c.recordTool(a, call, toolErr); err != nil {
		return "", err
	}
	return redact(output), nil
}
