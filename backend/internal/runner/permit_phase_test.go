package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type phaseAuthorizerFunc func(context.Context, PermitRequest) (Permit, error)

func (f phaseAuthorizerFunc) Authorize(ctx context.Context, req PermitRequest) (Permit, error) {
	return f(ctx, req)
}

func TestAdmissionSettingsChangedIsRetryableConflict(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	c.authorizer = phaseAuthorizerFunc(func(_ context.Context, req PermitRequest) (Permit, error) {
		if req.Phase != "admit" {
			t.Errorf("initial callback phase = %q", req.Phase)
		}
		return Permit{Allowed: false, Reason: "settings_changed", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	spec := specFor("test")
	if _, err := c.Start(context.Background(), spec); !errors.Is(err, ErrConflict) {
		t.Fatalf("settings rejection did not map to conflict: %v", err)
	}
	if _, err := c.Inspect(spec.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatal("rejected admission registered a run", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 {
		t.Fatal("rejected admission created a container")
	}
}

func TestAuthorizationPhasesDistinguishPreparationAndRenewal(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	fake.runningTest = true
	var mu sync.Mutex
	seen := map[string]int{}
	renewed := make(chan struct{}, 1)
	c.authorizer = phaseAuthorizerFunc(func(_ context.Context, req PermitRequest) (Permit, error) {
		mu.Lock()
		seen[req.Phase]++
		mu.Unlock()
		if req.Phase == "renew" {
			select {
			case renewed <- struct{}{}:
			default:
			}
		}
		return Permit{Allowed: true, ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	spec := specFor("test")
	spec.Limits.TimeoutSeconds = 0
	if _, err := c.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	select {
	case <-renewed:
	case <-time.After(time.Second):
		t.Fatal("no renewal callback")
	}
	if _, err := c.Stop(context.Background(), spec.RunID); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, c, spec.RunID)
	mu.Lock()
	defer mu.Unlock()
	if seen["admit"] != 1 || seen["prepare"] != 1 || seen["renew"] < 1 || len(seen) != 3 {
		t.Fatalf("unexpected callback phases: %v", seen)
	}
}
