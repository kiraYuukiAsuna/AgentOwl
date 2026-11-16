package service

import "context"

func (s *Service) RuntimeMachineSecret(ctx context.Context, id string) (Data, Data, error) {
	return s.config(ctx, s.DB, id, "runtime-machines")
}
