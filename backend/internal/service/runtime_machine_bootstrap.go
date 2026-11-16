package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// bootstrapRuntimeMachine runs only with first-administrator initialization.
func (s *Service) bootstrapRuntimeMachine(ctx context.Context, tx pgx.Tx) error {
	id := s.Config.BootstrapRuntimeMachineID
	if id == "" {
		return nil
	}
	if len(s.Config.BootstrapRuntimeToken) < 32 || len(s.Config.BootstrapRuntimeCallbackToken) < 32 {
		return errors.New("local runtime bootstrap requires service and callback tokens of at least 32 characters")
	}
	data := Data{"name": "本机", "base_url": "http://runner-controller:8090", "callback_base_url": "http://api:8081", "status": "active", "scheduling_weight": 1, "allowed_images": []string{}}
	encrypted, err := s.Vault.Seal(JSON(Data{"token": s.Config.BootstrapRuntimeToken, "callback_token": s.Config.BootstrapRuntimeCallbackToken}), []byte("runtime-machines:"+id))
	if err != nil {
		return err
	}
	// An existing machine pool is managed by the administrator.
	_, err = tx.Exec(ctx, `WITH inserted AS (
 INSERT INTO configs(id,kind,name,data,secrets)
 SELECT $1,'runtime-machines','本机',$2,$3
 WHERE NOT EXISTS (SELECT 1 FROM configs WHERE kind='runtime-machines')
 ON CONFLICT DO NOTHING RETURNING id,version,data
 ) INSERT INTO config_versions(config_id,version,data,actor_id)
 SELECT id,version,data,'bootstrap' FROM inserted`, id, JSON(data), encrypted)
	return err
}
