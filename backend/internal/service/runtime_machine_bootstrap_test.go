package service

import (
	"context"
	"strings"
	"testing"
)

func TestLocalRuntimeBootstrap(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	// Simulate a fresh deployment before its first administrator is created.
	if _, err := s.DB.Exec(ctx, "DELETE FROM principals"); err != nil {
		t.Fatal(err)
	}
	s.Config.BootstrapRuntimeMachineID = ID()
	s.Config.BootstrapRuntimeToken = strings.Repeat("s", 32)
	s.Config.BootstrapRuntimeCallbackToken = strings.Repeat("c", 32)
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	machine, secret, err := s.RuntimeMachineSecret(ctx, s.Config.BootstrapRuntimeMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if Str(machine, "base_url") != "http://runner-controller:8090" || Str(secret, "token") != s.Config.BootstrapRuntimeToken || Str(secret, "callback_token") != s.Config.BootstrapRuntimeCallbackToken {
		t.Fatalf("unexpected bootstrap: %v", machine)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE configs SET name='custom',data=jsonb_set(data,'{status}','"disabled"') WHERE id=$1`, s.Config.BootstrapRuntimeMachineID); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	machine, _, err = s.RuntimeMachineSecret(ctx, s.Config.BootstrapRuntimeMachineID)
	if err != nil || Str(machine, "name") != "custom" || Str(machine, "status") != "disabled" {
		t.Fatalf("bootstrap overwrote edits: %v %v", machine, err)
	}
	if _, err := s.DB.Exec(ctx, "DELETE FROM config_versions; DELETE FROM configs"); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	machines, err := s.runtimeMachines(ctx)
	if err != nil || len(machines) != 0 {
		t.Fatalf("deleted machine recreated: %v %v", machines, err)
	}
}

func TestLocalRuntimeBootstrapPreservesExistingPool(t *testing.T) {
	s, _ := testService(t)
	existing := supportRuntimeMachine(t, s, "http://existing:8090")
	if _, err := s.DB.Exec(context.Background(), "DELETE FROM principals"); err != nil {
		t.Fatal(err)
	}
	s.Config.BootstrapRuntimeMachineID = ID()
	s.Config.BootstrapRuntimeToken = strings.Repeat("s", 32)
	s.Config.BootstrapRuntimeCallbackToken = strings.Repeat("c", 32)
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	machines, err := s.runtimeMachines(context.Background())
	if err != nil || len(machines) != 1 || Str(machines[0], "id") != existing {
		t.Fatalf("existing pool changed: %v %v", machines, err)
	}
}
