package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"agentowl/backend/internal/httpapi"
	"agentowl/backend/internal/platform"
	"agentowl/backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Services struct {
	Config platform.Config
	DB     *pgxpool.Pool
	Core   *service.Service
}

func NewServices(ctx context.Context, cfg platform.Config) (*Services, error) {
	db, err := platform.Open(ctx, cfg)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	core, err := service.New(db, cfg)
	if err != nil {
		db.Close()
		return nil, errors.New("service configuration invalid")
	}
	return &Services{Config: cfg, DB: db, Core: core}, nil
}

func (s *Services) Close() { s.DB.Close() }

type API struct {
	Services *Services
	Server   *httpapi.Server
}

func NewAPI(ctx context.Context, cfg platform.Config) (*API, error) {
	services, err := NewServices(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err = services.Core.Bootstrap(ctx); err != nil {
		services.Close()
		return nil, err
	}
	return &API{Services: services, Server: httpapi.New(services.Core)}, nil
}

func (a *API) HTTPServers() []*http.Server {
	cfg := a.Services.Config
	return []*http.Server{{Addr: cfg.Listen, Handler: a.Server.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16384}, {Addr: cfg.InternalListen, Handler: a.Server.InternalHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}}
}

type Worker struct{ Services *Services }

func NewWorker(ctx context.Context, cfg platform.Config) (*Worker, error) {
	services, err := NewServices(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Worker{Services: services}, nil
}

func (w *Worker) Run(ctx context.Context) error { return w.Services.Core.RunWorker(ctx) }
