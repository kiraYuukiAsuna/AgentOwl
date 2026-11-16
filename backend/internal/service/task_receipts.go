package service

import (
	"agentowl/backend/internal/runner"
	"context"
	"encoding/json"
	"fmt"
)

func (s *Service) recordTaskInputReceipts(ctx context.Context, q DB, spec runner.RunSpec, remote runner.Run) error {
	if spec.TaskInputProtocol == 0 {
		return nil
	}
	if remote.RunID != spec.RunID || remote.SpecDigest == "" {
		return Fail(422, "invalid_receipt", "执行回执不匹配")
	}
	digest, e := runner.SpecDigest(spec)
	if e != nil {
		return e
	}
	if digest != remote.SpecDigest {
		return Conflict
	}
	for _, receipt := range remote.Result.InputReceipts {
		found := false
		for _, in := range spec.TaskInputs {
			if in.ID == receipt.InputID && in.Digest == receipt.Digest {
				found = true
			}
		}
		if !found || !contains([]string{"delivered", "included"}, receipt.Phase) {
			return Fail(422, "invalid_receipt", "输入回执无效")
		}
		if receipt.Phase == "included" {
			if _, e = runner.TaskInputPage(spec.TaskInputs, receipt.InputID, receipt.PartID); e != nil {
				return e
			}
		} else if receipt.PartID != "all" {
			return Fail(422, "invalid_receipt", "送达回执无效")
		}
		key := fmt.Sprintf("%s:%s:%s:%s", spec.RunID, receipt.InputID, receipt.Phase, receipt.PartID)
		if _, e = q.Exec(ctx, `INSERT INTO run_input_receipts(run_id,input_id,phase,content_digest,part_id,receipt_key) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, spec.RunID, receipt.InputID, receipt.Phase, receipt.Digest, receipt.PartID, key); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) TaskInputPage(ctx context.Context, runID, inputID, cursor string) (Data, error) {
	if !validID(runID) || !validID(inputID) {
		return nil, NotFound
	}
	r, e := one(ctx, s.DB, "SELECT to_jsonb(r) FROM run_attempts r WHERE id=$1", runID)
	if e != nil {
		return nil, e
	}
	if !contains([]string{"queued", "running"}, Str(r, "status")) {
		return nil, Conflict
	}
	t, e := one(ctx, s.DB, "SELECT to_jsonb(t) FROM dev_tasks t WHERE id=$1", Str(r, "task_id"))
	if e != nil {
		return nil, e
	}
	if e = s.validateTaskMandate(ctx, s.DB, t); e != nil {
		return nil, e
	}
	if Num(r, "epoch") != Num(t, "epoch") {
		return nil, Conflict
	}
	var spec runner.RunSpec
	if e = json.Unmarshal(JSON(r["spec"]), &spec); e != nil {
		return nil, e
	}
	page, e := runner.TaskInputPage(spec.TaskInputs, inputID, cursor)
	return decodeData(page), e
}

func (s *Service) CheckRunMachine(ctx context.Context, runID, machineID string) error {
	if !validID(runID) || !validID(machineID) {
		return Forbidden
	}
	var allowed bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_attempts WHERE id=$1 AND runtime_machine_id=$2)`, runID, machineID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return Forbidden
	}
	return nil
}
