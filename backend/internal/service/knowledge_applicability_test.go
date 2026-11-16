package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentowl/backend/internal/integration"
	"github.com/jackc/pgx/v5"
)

func TestKnowledgeSemanticVersionApplicability(t *testing.T) {
	for _, tc := range []struct {
		constraint, version string
		match               bool
	}{
		{"", "", true}, {"1.2.3", "v1.2.3", true}, {"v1.2.3+one", "1.2.3+two", true}, {">=v1.2.3, <2.0.0", "1.10.0", true}, {">= 1.2.3 < 2.0.0", "2.0.0", false}, {">1.2.3 <=2.0.0", "1.2.3", false}, {">1.2.3 <=2.0.0", "2.0.0", true},
		{">=1.0.0 <2.0.0", "1.3.0-rc.1", false}, {"1.2.3", "1.2.3-rc.1", false}, {"1.2.3-rc.1", "v1.2.3-rc.1+build", true}, {">=1.2.3-beta.2 <1.2.3", "1.2.3-rc.1", true}, {">=1.2.3-beta.2 <1.2.3", "1.2.3-beta.1", false}, {">=1.2.3-beta.2 <2.0.0", "1.3.0-beta.5", false}, {">1.2.3-alpha.2 <1.2.3", "1.2.3-alpha.10", true}, {">1.2.3-alpha.9 <1.2.3", "1.2.3-alpha.beta", true}, {">1.2.3-alpha <1.2.3", "1.2.3-alpha.1", true}, {">999999999999999999999.0.0", "1000000000000000000000.0.0", true},
		{">=1.2.3", "", false}, {">=1.2.3", "invalid", false},
	} {
		t.Run(tc.constraint+"_"+tc.version, func(t *testing.T) {
			e := knowledgeApplicability(Data{"cli_version": tc.constraint}, Data{"environment": Data{"cli_version": tc.version}})
			if (e == nil) != tc.match {
				t.Fatalf("match=%v expected=%v error=%v", e == nil, tc.match, e)
			}
		})
	}
	for _, invalid := range []string{"^1.2.3", "~1.2.3", "1.2.x", "1.2", "1.2.3 || 2.0.0", ">=1.2.3,,<2.0.0", ",1.2.3", "1.2.3,", ">=", "01.2.3", "1.2.3-alpha.01", "1.2.3+", "1.2.3 - 2.0.0"} {
		if e := validateKnowledgeApplicability(Data{"cli_version": invalid}); e == nil {
			t.Fatalf("unsupported range accepted: %s", invalid)
		}
	}
}

func TestKnowledgeEnvironmentApplicabilityAndExplicitFacts(t *testing.T) {
	cases := []struct {
		name                string
		knowledge, question Data
		match               bool
	}{
		{"aliases", Data{"cli_version": ">=1.2.0 <2.0.0", "environment": "macOS/production/keyless"}, Data{"environment": Data{"cli_version": "v1.2.3", "os": "darwin", "environment": "生产", "auth_mode": "keyless"}}, true},
		{"missing-version", Data{"cli_version": ">=1.0.0"}, Data{"description": "error number 1.2.3"}, false},
		{"explicit-labeled", Data{"cli_version": "1.2.3", "environment": "darwin/BOE/keyless"}, Data{"description": "CLI version: v1.2.3 on macOS in BOE. auth mode: keyless"}, true},
		{"chinese-labeled", Data{"cli_version": "1.2.3", "environment": "macOS/prod/keyless"}, Data{"description": "CLI版本：1.2.3，macOS，生产环境；认证方式：keyless"}, true},
		{"lark-cli-labeled", Data{"cli_version": "1.2.3", "environment": "linux/test"}, Data{"description": "lark-cli version 1.2.3 on Linux, environment: test"}, true},
		{"unlabelled-test-not-env", Data{"environment": "test"}, Data{"description": "the unit test failed"}, false},
		{"missing-auth", Data{"environment": "keyless"}, Data{"description": "DPoP/keyless documentation question"}, false},
		{"ambiguous-version", Data{"cli_version": ">=1.0.0 <3.0.0"}, Data{"description": "CLI version 1.2.3 and CLI version 2.0.0"}, false},
		{"ambiguous-os", Data{"environment": "macOS"}, Data{"description": "macOS and Linux behave differently"}, false},
		{"ambiguous-env", Data{"environment": "boe"}, Data{"description": "BOE versus production"}, false},
		{"alternative-env", Data{"environment": "boe"}, Data{"description": "BOE/test"}, false},
		{"alternative-version", Data{"cli_version": "1.2.3"}, Data{"description": "CLI version 1.2.3/2.0.0"}, false},
		{"negated-os", Data{"environment": "windows"}, Data{"description": "not Windows"}, false},
		{"explicit-priority", Data{"cli_version": "1.2.3", "environment": "linux/boe"}, Data{"description": "CLI version 2.0.0 on macOS production previously", "environment": Data{"cli_version": "1.2.3", "os": "linux", "environment": "boe"}}, true},
		{"invalid-explicit-does-not-fallback", Data{"environment": "linux"}, Data{"description": "Linux", "environment": Data{"os": "plan9"}}, false},
		{"different-version", Data{"cli_version": "<2.0.0"}, Data{"environment": Data{"cli_version": "2.0.0"}}, false},
		{"different-os", Data{"environment": "macOS"}, Data{"environment": Data{"os": "windows"}}, false},
		{"unknown-knowledge-environment", Data{"environment": "planet-x"}, Data{"environment": Data{"environment": "planet-x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := knowledgeApplicability(tc.knowledge, tc.question)
			if (e == nil) != tc.match {
				t.Fatalf("match=%v expected=%v error=%v", e == nil, tc.match, e)
			}
		})
	}
	for _, env := range []string{"linux/windows", "BOE/test", "macOS/", "custom", "keyless/DPoP"} {
		if e := validateKnowledgeApplicability(Data{"environment": env}); e == nil {
			t.Fatalf("unsupported/contradictory environment accepted: %s", env)
		}
	}
}

func TestKnowledgeApplicabilitySaveAndSendGate(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	for _, bad := range []Data{{"cli_version": "^1.0.0"}, {"environment": "macOS/windows"}} {
		bad["title"] = "invalid"
		bad["body"] = "body"
		e := s.transaction(ctx, func(tx pgx.Tx) error { _, e := s.SaveKnowledge(ctx, tx, a, "", bad); return e })
		if e == nil {
			t.Fatal("invalid applicability saved")
		}
	}
	k := supportKnowledge(t, s, "public", "Version-specific answer")
	if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET cli_version='>=1.0.0 <2.0.0',environment='macOS/BOE/keyless' WHERE id=$1", k); e != nil {
		t.Fatal(e)
	}
	c := Data{"environment": Data{"cli_version": "1.2.3", "os": "darwin", "environment": "boe", "auth_mode": "keyless"}}
	if _, e := s.evidence(ctx, s.DB, c, []string{k}); e != nil {
		t.Fatal(e)
	}
	Object(c, "environment")["cli_version"] = "2.0.0"
	if _, e := s.evidence(ctx, s.DB, c, []string{k}); e == nil {
		t.Fatal("cross-version evidence passed send-time gate")
	}
	delete(Object(c, "environment"), "cli_version")
	if _, e := s.evidence(ctx, s.DB, c, []string{k}); e == nil || !strings.Contains(e.Error(), "请补充") {
		t.Fatalf("missing version should request info: %v", e)
	}
}

func TestKnowledgeApplicabilityRetrievalSkipsWrongTopCandidates(t *testing.T) {
	s, a := testService(t)
	ctx := context.Background()
	_, ch := supportLarkFixture(t, s)
	c := supportCase(t, s, a, ch)
	if _, e := s.DB.Exec(ctx, "UPDATE cases SET environment='{\"cli_version\":\"2.1.0\",\"os\":\"linux\",\"environment\":\"boe\"}'::jsonb WHERE id=$1", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	good := supportKnowledge(t, s, "public", "APPLICABLE_GUIDANCE")
	if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET cli_version='>=2.0.0 <3.0.0',environment='linux/BOE' WHERE id=$1", good); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 12; i++ {
		bad := supportKnowledge(t, s, "public", "WRONG_VERSION_GUIDANCE")
		if _, e := s.DB.Exec(ctx, "UPDATE knowledge SET cli_version='<2.0.0' WHERE id=$1", bad); e != nil {
			t.Fatal(e)
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "WRONG_VERSION_GUIDANCE") {
			t.Error("wrong-version evidence reached model")
		}
		if !strings.Contains(string(body), "APPLICABLE_GUIDANCE") {
			t.Error("valid candidate lost behind invalid top results")
		}
		supportModelResponse(w, Data{"disposition": "answer", "text": "Applicable answer", "citation_ids": []string{good}, "missing_info": []string{}})
	}))
	defer server.Close()
	model, e := integration.NewModelClient(integration.ModelConfig{BaseURL: server.URL, Model: "test"})
	if e != nil {
		t.Fatal(e)
	}
	s.Model = model
	reply := supportReply(t, s, c)
	if e = s.ProcessSupportJob(ctx, "answer", Data{"reply_id": reply}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE id=$1 AND status='draft'", reply); n != 1 {
		t.Fatal(n)
	}
	if _, e = s.DB.Exec(ctx, "UPDATE cases SET environment='{}'::jsonb WHERE id=$1", Str(c, "id")); e != nil {
		t.Fatal(e)
	}
	reply = supportReply(t, s, c)
	if e = s.ProcessSupportJob(ctx, "answer", Data{"reply_id": reply}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("model was asked to decide applicability")
	}
	if n := supportCount(t, s, "SELECT count(*) FROM replies WHERE id=$1 AND status='failed'", reply); n != 1 {
		t.Fatal(n)
	}
	var events []byte
	if e = s.DB.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(e)) FROM case_events e WHERE case_id=$1", Str(c, "id")).Scan(&events); e != nil {
		t.Fatal(e)
	}
	if !json.Valid(events) || !strings.Contains(string(events), "请补充") {
		t.Fatal("missing facts did not create actionable clarification")
	}
}
