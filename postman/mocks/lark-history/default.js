const http = require("http");
http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  const reply = (status, data) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(data)); };
  if (req.method === "POST" && url.pathname === "/open-apis/auth/v3/tenant_access_token/internal")
    return reply(200, { code: 0, tenant_access_token: "synthetic-token", expire: 7200 });
  if (req.method === "GET" && url.pathname === "/open-apis/im/v1/messages" && url.searchParams.get("container_id_type") === "chat" && url.searchParams.get("container_id") === "oc_test")
    return reply(200, { code: 0, data: { items: [{ message_id: "om_fixture", chat_id: "oc_test", msg_type: "text", create_time: "1760000000000", body: { content: '{"text":"auth help"}' }, sender: { id: "ou_fixture", sender_type: "user" } }], has_more: false } });
  return reply(501, { code: 1, msg: "unmodeled endpoint" });
}).listen(process.env.PORT, "127.0.0.1");
