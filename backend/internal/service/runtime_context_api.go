package service

import "context"

func (s *Service) RunnerContextManifest(ctx context.Context, runID string) (Data, error) {
	manifest, err := RuntimeContextProvider{Service: s}.ContextManifest(ctx, runID)
	if err != nil {
		return nil, err
	}
	return decodeData(manifest), nil
}

func (s *Service) RunnerContextRef(ctx context.Context, runID, refID string) (string, error) {
	return RuntimeContextProvider{Service: s}.ContextRef(ctx, runID, refID)
}

func (s *Service) RunnerContextPage(ctx context.Context, runID, refID, cursor string) (Data, error) {
	page, err := (RuntimeContextProvider{Service: s}).ContextRefPage(ctx, runID, refID, cursor)
	return decodeData(page), err
}
