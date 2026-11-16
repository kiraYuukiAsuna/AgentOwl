package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

type ContextProvider interface {
	ContextManifest(context.Context, string) (ContextManifest, error)
	ContextRef(context.Context, string, string) (string, error)
}

type PagedContextProvider interface {
	ContextRefPage(context.Context, string, string, string) (ContextPage, error)
}

type ContextPage struct {
	ID              string `json:"id"`
	Digest          string `json:"content_digest"`
	Text            string `json:"text"`
	Offset          int    `json:"offset"`
	TotalCharacters int    `json:"total_characters"`
	NextCursor      string `json:"next_cursor,omitempty"`
	NextTool        string `json:"next_tool,omitempty"`
}

func PageContext(id, body, cursor string) (ContextPage, error) {
	h := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(h[:])
	start := 0
	if cursor != "" {
		version, offset, ok := strings.Cut(cursor, ":")
		n, err := strconv.Atoi(offset)
		if !ok || version != digest || err != nil || n < 0 || n%1536 != 0 {
			return ContextPage{}, errors.New("invalid context cursor")
		}
		start = n
	}
	chars := []rune(body)
	if start > len(chars) {
		return ContextPage{}, errors.New("invalid context cursor")
	}
	end := min(start+1536, len(chars))
	page := ContextPage{ID: id, Digest: digest, Text: string(chars[start:end]), Offset: start, TotalCharacters: len(chars)}
	if end < len(chars) {
		page.NextCursor = digest + ":" + strconv.Itoa(end)
		page.NextTool = "read_context_ref"
	}
	return page, nil
}

func contextPrompt(spec RunSpec) string {
	if spec.ContextManifest.Version < 2 {
		return taskInputPrompt(spec)
	}
	header, _ := json.Marshal(spec.ContextManifest)
	return "\n<agentowl_context>\n" + string(header) + "\n</agentowl_context>"
}

var contextEncodingOnce sync.Once
var contextEncoding *tiktoken.Tiktoken
var contextEncodingErr error

func contextTokenizer() (*tiktoken.Tiktoken, error) {
	contextEncodingOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		contextEncoding, contextEncodingErr = tiktoken.GetEncoding("o200k_base")
	})
	return contextEncoding, contextEncodingErr
}

func EstimateTextTokens(text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	enc, err := contextTokenizer()
	if err != nil {
		return 0, err
	}
	return len(enc.Encode(text, nil, nil)), nil
}

func EstimateJSONTokens(value any) (int, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return EstimateTextTokens(string(body))
}

func TailTextTokens(text string, max int) (string, error) {
	if max <= 0 || text == "" {
		return "", nil
	}
	enc, err := contextTokenizer()
	if err != nil {
		return "", err
	}
	tokens := enc.Encode(text, nil, nil)
	if len(tokens) <= max {
		return text, nil
	}
	return enc.Decode(tokens[len(tokens)-max:]), nil
}
