-- Runtime configuration is managed in configs; retain obsolete rows for audit.
INSERT INTO configs(id,kind,name,data)
SELECT id,'runtime-machines',name,
 jsonb_build_object('base_url',base_url,'callback_base_url',callback_base_url,
 'status','disabled','allowed_images',allowed_images,'scheduling_weight',scheduling_weight,
 'legacy_runtime_machine',true)
FROM runtime_machines ON CONFLICT(id) DO NOTHING;
ALTER TABLE run_attempts DROP CONSTRAINT run_attempts_runtime_machine_id_fkey;
ALTER TABLE run_attempts ADD CONSTRAINT run_attempts_runtime_machine_id_fkey FOREIGN KEY(runtime_machine_id) REFERENCES configs(id);
