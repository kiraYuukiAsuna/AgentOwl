.PHONY: help init bootstrap dev check ci gen-client migrate up down runner-preflight dev-services backup restore

help:
	@printf '%s\n' 'init              Create private local configuration (no secrets printed)' 'bootstrap         Install Go and frontend dependencies' 'dev-services      Start PostgreSQL at 127.0.0.1:55432 and wait for readiness' 'dev               Run local API, Worker and Next.js' 'check             Check source, tests, contracts and available deployment tools' 'ci                Full checks including PostgreSQL, Compose and generated-client drift' 'gen-client        Regenerate TypeScript client with Kiota 1.36.1' 'migrate           Apply SQL migrations explicitly in Docker' 'up                Start application services (host Nginx is separate)' 'down              Stop all application profiles without deleting data' 'runner-preflight  Verify the dedicated Linux runtime (requires socket access)' 'backup            Linux cold backup; requires root and BACKUP_OUTPUT' 'restore           Linux cold restore; requires root and BACKUP_ARCHIVE'

init:
	@script/init-env.sh

bootstrap:
	cd backend && go mod download
	cd Frontend && npm ci --ignore-scripts --no-audit --no-fund

dev:
	@script/dev.sh

check:
	@script/check.sh

ci:
	@script/check.sh --ci

gen-client:
	@script/gen-client.sh

migrate:
	docker compose --env-file deploy/.env -f deploy/compose.yaml up -d postgres
	docker compose --env-file deploy/.env -f deploy/compose.yaml run --rm --build migrate

up:
	docker compose --env-file deploy/.env -f deploy/compose.yaml up -d --build

down:
	docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner down

runner-preflight:
	@script/runner-preflight.sh --data-root "$${RUNNER_HOST_DATA_ROOT:?Set RUNNER_HOST_DATA_ROOT}" --image "$${RUNNER_TEST_IMAGE:?Set a digest-pinned RUNNER_TEST_IMAGE}"

backup:
	@script/backup.sh --output "$${BACKUP_OUTPUT:?Set an absolute new backup archive path}"

restore:
	@script/restore.sh --archive "$${BACKUP_ARCHIVE:?Set the absolute backup archive path}"

dev-services:
	docker compose --env-file deploy/.env -f deploy/compose.yaml -f deploy/compose.dev.yaml up -d --wait postgres
