const http = require("http");
http.createServer((req, res) => {
  res.setHeader("Content-Type", "application/json");
  if (req.method === "POST" && req.url === "/open-apis/auth/v3/tenant_access_token/internal")
    return res.end(JSON.stringify({ code: 0, tenant_access_token: "synthetic-token", expire: 7200 }));
  if (req.method === "GET" && req.url.startsWith("/open-apis/im/v1/messages?")) {
    res.statusCode = 400;
    return res.end(JSON.stringify({ code: 230002, msg: "Bot/User can NOT be out of the chat.", error: { log_id: "synthetic-log-id" } }));
  }
  res.statusCode = 501;
  return res.end(JSON.stringify({ code: 1, msg: "unmodeled endpoint" }));
}).listen(process.env.PORT, "127.0.0.1");
