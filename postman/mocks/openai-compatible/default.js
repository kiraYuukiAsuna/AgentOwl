// Contract: backend/internal/integration/model.go, ModelClient.Classify/Complete.
const http = require("http");

http.createServer((req, res) => {
  const reply = (status, body) => {
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  };
  if (req.method !== "POST" || req.url !== "/v1/chat/completions") {
    return reply(501, { error: { code: "unmodeled_endpoint" } });
  }
  const body = req.body;
  if (body?.model === "agentowl-unlimited-simulation") {
    if (body.stream !== false || body.max_tokens !== 64 || body.messages?.length !== 1 ||
        body.messages[0].role !== "user" || typeof body.messages[0].content !== "string" ||
        body.messages[0].content.length <= 1024) {
      return reply(400, { error: { code: "unexpected_large_request" } });
    }
    return reply(200, {
      choices: [{ finish_reason: "stop", message: { role: "assistant", content: "large request received" } }],
      usage: { prompt_tokens: 32, completion_tokens: 8, total_tokens: 40 },
    });
  }
  if (!body || body.model !== "agentowl-simulation" || body.stream !== false ||
      body.max_tokens !== 1500 || body.max_completion_tokens !== undefined ||
      !Array.isArray(body.messages) || body.messages.length !== 2 ||
      body.messages[0].role !== "system" || body.messages[1].role !== "user" ||
      body.response_format?.type !== "json_schema" ||
      body.response_format?.json_schema?.name !== "support_classification" ||
      body.response_format?.json_schema?.strict !== true ||
      body.response_format?.json_schema?.schema?.additionalProperties !== false) {
    return reply(400, { error: { code: "unexpected_classification_request" } });
  }
  reply(200, {
    id: "agentowl-simulation-classification",
    object: "chat.completion",
    model: body.model,
    choices: [{
      index: 0,
      finish_reason: "stop",
      message: {
        role: "assistant",
        content: JSON.stringify({
          support_relevance: "in_scope",
          type: "question",
          module: "auth",
          reason: "Local simulation fixture matched the classification request.",
          missing_info: [],
          needs_human: false,
        }),
      },
    }],
    usage: { prompt_tokens: 64, completion_tokens: 24, total_tokens: 88 },
  });
}).listen(process.env.PORT, "127.0.0.1");
