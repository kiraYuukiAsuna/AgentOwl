# Model settings simulation

Created 2026-10-28 from the request and response contract in `backend/internal/integration/model.go`. No external workspace was searched; the task explicitly limits this fixture to local code.

## Purpose

Exercise database-backed `CurrentModel` classification and the separation of model request size from response limits against a Postman HTTP server. The handler implements only the two requests covered by the opt-in tests.

## Execution

From the repository root:

```sh
postman simulation run postman/simulations/model-settings/model-settings.sim.yaml --port auto --output ndjson --no-history > /tmp/agentowl-model-sim.ndjson 2> /tmp/agentowl-model-sim.err
```

Run the server in a separate terminal. Take the member URL from its `listening` event, append `/v1`, and set `AGENTOWL_POSTMAN_MODEL_URL` for:

```sh
cd backend
go test ./internal/service -run '^TestModelSettingsPostmanSimulation$' -count=1 -v
```

The test also requires `AGENTOWL_TEST_DATABASE_URL`; it creates and removes its own schema. Verify a `request` event for the model member, then stop the simulation with Ctrl-C. Keep logs outside the repository.

`go test ./internal/integration -run '^TestModelRequestBudgetPostmanSimulation$' -count=1 -v` uses the same `AGENTOWL_POSTMAN_MODEL_URL` and needs no database. It sends synthetic 8 KiB context with a 1 KiB response limit.

## Assumptions

- This fixture accepts only loopback URLs and uses synthetic input and a dummy API Key; it does not establish any real provider's compatibility. (reasoned 2026-10-28, test and handler)
- CLI 1.73.0 successfully ran this local manifest with `--no-history`, without a workspace or real model credential. The listener was confirmed on `127.0.0.1`; SIGINT stopped it and removed the listener. (observed 2026-10-28)

## Observations

<!-- begin:observations -->
2026-11-02: After the AgentOwl rename, `TestModelSettingsPostmanSimulation` and `TestModelRequestBudgetPostmanSimulation` passed under Linux with the real client, an isolated PostgreSQL database and CLI 1.73.0. The model member recorded both `POST /v1/chat/completions` requests with HTTP 200. This verifies the renamed configuration/client path against the local fixture; no real model provider was called.
2026-10-29: the model member served the real client's request-budget probe at `POST /v1/chat/completions` with HTTP 200; no member stayed silent. The opt-in integration test passed. This establishes separate request/response limits against the local fixture, not provider context-window capacity.
2026-10-28: the model member served the real database-configured client's `POST /v1/chat/completions` with HTTP 200. `TestModelSettingsPostmanSimulation` passed its classification and usage checks; no member stayed silent. This proves the configured client path against the local fixture, not a real model provider.
<!-- end:observations -->

## What didn't work

- 2026-10-29: a 5 MiB probe received HTTP 502 before any member request event; the simulation emitted no diagnostic explaining the cause. The 8 KiB probe passed after restart. The independent real-HTTP test `TestModelRequestSizeIsIndependentOfResponseLimit` covers 5 MiB delivery and response-limit enforcement; do not claim that payload size passed this simulation.
- 2026-10-28: CLI 1.73.0 wrote the local template, then exited with `ERROR: SecItemCopyMatching failed -50` during `mock generate`. No external workspace was used.
