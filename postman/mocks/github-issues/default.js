// Minimal local fixture for the GitHub Issue and comment list contract.
const http = require("http");
http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  const reply = (status, body) => {
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  };
  if (req.method !== "GET" || !url.searchParams.get("since") || url.searchParams.get("per_page") !== "100")
    return reply(400, { message: "expected a paginated incremental GET" });
  const when = new Date(Date.now() - 120000).toISOString();
  if (url.pathname === "/repos/owner/repo/issues")
    return reply(200, [{ id: 42, number: 7, title: "Test issue", body: "Test body", user: { id: 24, type: "User" }, created_at: when, updated_at: when }]);
  if (url.pathname === "/repos/owner/repo/issues/7/comments")
    return reply(200, [{ id: 43, body: "Test comment", user: { id: 25, type: "User" }, created_at: when, updated_at: when }]);
  return reply(501, { message: "unmodeled endpoint" });
}).listen(process.env.PORT, "127.0.0.1");
