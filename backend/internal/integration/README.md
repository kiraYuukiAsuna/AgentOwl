# External Platform Adapters

This package implements pure HTTP protocol adapters for Lark/Feishu, GitHub, and OpenAI-compatible model endpoints.

By design, adapters have no database access and do not handle business authorization, job persistence, tool execution, or write retries. Callers (the `service` layer) are responsible for channel/repository permissions, approval version checks, durable idempotency, and token budgets.

## Lark / Feishu

- **Auth & tokens**: `LarkConfig.BaseURL` selects the regional API host. Tenant access tokens are acquired via the self-built app endpoint (matching the [official Go SDK](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/service/auth/v3/resource.go)) and cached with a safety margin using single-flight refresh. Token-invalid errors evict the cache immediately without silently replaying the failed call.
- **Webhooks & card callbacks**:
  - `ParseWebhook` handles v2 HTTP events (verifying `AppID`, `TenantKey`, `VerificationToken`, and `EncryptKey`) and also accepts card button callbacks on the same route for single-endpoint deployments.
  - `ParseCardAction` serves the dedicated `/actions` endpoint and accepts both legacy token-signed and v2 card payloads. Both paths normalize into the same `CardAction` struct.
  - URL challenge handshakes respond only after credential verification; signatures, AES-CBC decryption, and clock skew checks match the official SDK.
  - `CardAction.ActorTenantID` identifies the clicking user's tenant, while `TenantID` identifies the app owner's tenant. Treat `Value` and `FormValue` as untrusted input — a valid signature only proves platform delivery, not business approval. Always map the operator to an internal member and check permissions, resource scope, and object version in `service`.
- **History & messages**:
  - `History` fetches a single bounded page; callers persist the cursor and fetch thread replies separately. Time filters are rejected for thread history per the [Lark message list API](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/list). User-scoped OAuth refresh is outside this bot client's scope.
  - `SendMessage` requires a caller-persisted UUID and never auto-retries. Known remote `message_id` values can be looked up directly; sends that fail ambiguously with only a client UUID cannot be reliably queried back, and platform UUID deduplication is time-bounded, so callers must maintain an external-operation ledger. See [Send Message API](https://open.feishu.cn/document/server-docs/im-v1/message/create).
  - Recalled message events may omit `chat_id`; resolve the existing source record before running channel authorization. `Ignored` denotes an authenticated event type that AgentOwl does not consume.

## GitHub

- **Scope & webhooks**: Each client is scoped to one `owner/repo`. Webhooks are verified with HMAC-SHA256 over the raw request body ([GitHub docs](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)). Because delivery headers are not covered by the body signature, `Event.ID` is derived from the signed body's SHA-256 while `DeliveryID` is kept for troubleshooting. Persist event/message IDs to block replays; do not rely on `updated_at` for ordering — delete events get a distinct revision, and ambiguous ordering requires an authoritative fetch.
- **REST & CI checks**: Pinned to API version `2022-11-28` ([supported through March 10, 2028](https://docs.github.com/en/rest/about-the-rest-api/api-versions)). List endpoints return one bounded page per call. Both commit statuses and check runs must be read to evaluate CI state.
- **Git Database writes**: Blob reads verify SHA-1 and reject truncated trees. Branch updates never force-push. Creating Git objects is a remote write and must only run after approval, with allowed-branch/path checks and manifest verification in the publisher ([trees](https://docs.github.com/en/rest/git/trees), [blobs](https://docs.github.com/en/rest/git/blobs), [refs](https://docs.github.com/en/rest/git/refs)).
- **Reconciliation**: Issue, comment, and PR creation endpoints are not idempotent. Use correlation markers and PR queries to reconcile uncertain outcomes; an absent lookup does not prove a prior request never took effect. Keep `APIError.OutcomeUnknown` from triggering blind retries.

## Model Client & Secrets

- **Protocol**: `ModelClient` targets `BaseURL + /chat/completions` using the [OpenAI Chat Completions wire format](https://platform.openai.com/docs/api-reference/chat/create), with no vendor SDK lock-in or hardcoded default model.
- **Validation**: `Complete` validates tool names, JSON arguments, finish reasons, and token usage. `schema.go` implements an explicit JSON Schema subset and rejects unsupported keywords before sending. `Classify` and `Answer` enforce typed output schemas and disallow citing evidence IDs that were not supplied. Passing schema checks does not guarantee factual accuracy or permission to auto-send.
- **Usage & compatibility**: Providers must return `prompt_tokens`, `completion_tokens`, and `total_tokens`; missing usage is treated as an error rather than zero cost. Options like `json_object` and `max_completion_tokens` must be configured explicitly — there is no silent format fallback or automatic retry.
- **HTTP safety**: Requests enforce timeouts and body size limits, disable redirects on standard `http.Client` instances (custom `HTTPDoer` implementations must also honor cancellation and skip redirects), and surface bounded, redacted upstream diagnostics without leaking raw error bodies. Plaintext HTTP is allowed on loopback for local testing and requires opt-in for remote hosts.
- **Encryption & redaction**: `Vault` encrypts credentials with AES-256-GCM, fresh nonces, and caller-supplied associated data bound to credential owner and key version (key rotation is handled by deployment/service layers). `Redact` strips known secrets and common patterns as defense-in-depth, not as a complete DLP engine — avoid passing unnecessary source text or credentials into prompts and logs.

## Testing Scope

`integration_test.go` exercises local mock HTTP servers and security failure paths. It performs no live platform writes and does not replace live tenant, repository, or model-provider verification.
