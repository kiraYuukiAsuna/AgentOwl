// Package taskcore holds task rules without database, network or model dependencies.
package taskcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

var ErrState = errors.New("invalid task transition")
var ErrUnsettled = errors.New("execution unsettled")
var ErrStale = errors.New("stale task result")

type State struct {
	Status, Stage   string
	Revision, Epoch int64
}
type Command string

const (
	Confirm    Command = "confirm"
	Pause      Command = "pause"
	Cancel     Command = "cancel"
	Resume     Command = "resume"
	Revise     Command = "revise"
	Revoke     Command = "revoke"
	Advance    Command = "advance"
	Block      Command = "block"
	Prepared   Command = "prepared"
	Started    Command = "started"
	Publishing Command = "publishing"
	Published  Command = "published"
	Tracked    Command = "tracked"
	Retry      Command = "retry"
)

type Facts struct {
	Unsettled       bool
	Revision, Epoch int64
	NextStage       string
	Status          string
}

func Decide(s State, c Command, f Facts) (State, error) {
	next := s
	switch c {
	case Confirm:
		if s.Status != "" || s.Revision != 0 || s.Epoch != 0 {
			return s, ErrState
		}
		return State{Status: "queued", Stage: "plan", Revision: 1, Epoch: 1}, nil
	case Pause, Cancel, Revoke:
		if s.Status == "completed" || s.Status == "cancelled" {
			return s, ErrState
		}
		next.Status = "paused"
		if c == Cancel {
			next.Status = "cancelled"
		}
		if c == Revoke {
			next.Status = "blocked"
		}
		next.Epoch++
	case Resume:
		if s.Status != "paused" && s.Status != "blocked" && s.Status != "failed" {
			return s, ErrState
		}
		if f.Unsettled {
			return s, ErrUnsettled
		}
		next.Status = "queued"
		next.Epoch++
	case Revise:
		if s.Status == "cancelled" {
			return s, ErrState
		}
		if f.Unsettled || s.Status == "running" {
			return s, ErrUnsettled
		}
		next.Status = "queued"
		next.Stage = "plan"
		next.Revision++
		next.Epoch++
	case Advance:
		if f.Revision != s.Revision || f.Epoch != s.Epoch {
			return s, ErrStale
		}
		if s.Status == "paused" || s.Status == "cancelled" || s.Status == "completed" || s.Status == "blocked" {
			return s, ErrState
		}
		stages := []string{"plan", "edit", "test", "review", "publish_pr", "track"}
		valid := false
		for i := 0; i < len(stages)-1; i++ {
			if s.Stage == stages[i] && f.NextStage == stages[i+1] {
				valid = true
			}
		}
		if !valid {
			return s, ErrState
		}
		next.Status = "queued"
		next.Stage = f.NextStage
	case Block:
		if s.Status == "paused" || s.Status == "cancelled" || s.Status == "completed" {
			return s, ErrState
		}
		next.Status = "blocked"
	case Prepared:
		if s.Status != "queued" && s.Status != "pending_approval" {
			return s, ErrState
		}
		if f.Status != "pending_approval" && f.Status != "blocked" {
			return s, ErrState
		}
		next.Status = f.Status
		next.Stage = f.NextStage
	case Started, Publishing:
		if s.Status != "pending_approval" && s.Status != "queued" {
			return s, ErrState
		}
		next.Status = "running"
		if c == Publishing {
			if s.Stage != "publish_pr" {
				return s, ErrState
			}
			next.Status = "publishing"
		}
	case Published:
		if s.Status != "publishing" {
			return s, ErrState
		}
		if f.Status != "pr_open" && f.Status != "blocked" {
			return s, ErrState
		}
		next.Status = f.Status
		if f.Status == "pr_open" {
			next.Stage = "track"
		}
	case Tracked:
		if s.Status != "pr_open" || (f.Status != "pr_open" && f.Status != "completed" && f.Status != "blocked") {
			return s, ErrState
		}
		next.Status = f.Status
		next.Stage = "track"
	case Retry:
		if s.Status == "cancelled" || s.Status == "completed" || s.Status == "paused" || s.Status == "running" || s.Status == "publishing" {
			return s, ErrState
		}
		next.Status = "queued"
	default:
		return s, ErrState
	}
	return next, nil
}

type InputRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

func InputDigest(refs []InputRef) string {
	items := append([]InputRef{}, refs...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	b, _ := json.Marshal(struct {
		Version int        `json:"version"`
		Inputs  []InputRef `json:"inputs"`
	}{1, items})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
