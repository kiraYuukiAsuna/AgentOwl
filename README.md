# AgentOwl

AgentOwl 支持与开发工作台。把群消息、GitHub Issue 和人工反馈整理成问题，使用审核后的知识生成答复，将需要修改的问题交给开发者确认，再按审批策略执行开发、自测、审查和 PR 发布。

## 能力

- 飞书/Lark 应用机器人 HTTP 事件、加密验签、白名单渠道、历史与话题补采、消息编辑/撤回、交互卡片身份绑定。
- 飞书 / Lark 支持长连接接收群内 @ 机器人消息；群消息支持手动和定时历史同步；定时同步沿用手动处理方式。
- GitHub Issue、评论和 PR 事件接入；Issue 和评论可配置增量轮询，无需 Webhook 公网入口；基于固定快照发布 Draft PR；后续修改仍经过完整审批链，更新同一任务 PR；跟踪审查、CI 和合并状态。
- 可配置 OpenAI 兼容模型接口，结构化支持范围分类、有来源的回答、缺少证据时转人工、模块负责人路由和超时提醒；支持把当前问题里的授权图片附件作为多模态输入送入模型。
- 知识草稿、版本、发布审批、撤回、渠道可见范围、仓库 Markdown 与飞书文档同步；已发送问答可以整理成待审核知识。
- 开发流程 `plan → edit → test → review → publish_pr → track`，支持人工审批、规则审批、禁止、暂停、恢复、取消与修改目标。
- 本地或 OIDC 登录、角色和资源范围、审计、后台任务失败队列、外部写入结果核对、带权限和摘要校验的产物下载。

生产接入需要平台凭据、目标群权限、模型与 Linux Runner 配置。未配置模型时进入人工处理；未配置 Runner 时开发任务显示阻塞原因。真实平台及宿主验收边界见 [验证记录](docs/verification.md)。

## 项目结构

```text
Frontend/                 Next.js、React、TypeScript、Tailwind 4、Radix、SWR、Zustand
  app/                    工作台与配置页面
  components/             UI、表单和状态组件
  lib/api/generated/      Kiota 生成的 TypeScript 客户端
backend/
  api/openapi.yaml        OpenAPI 3.1 契约
  cmd/                    api、worker、runner-controller、migrate
  internal/service/       业务事务、审批、队列、知识、发布与产物
  internal/httpapi/       HTTP、会话、CSRF、内部运行许可
  internal/integration/   Lark、GitHub、模型、凭据加密
  internal/runner/        受限执行、Docker 协议与 Eino 开发 Agent
  internal/platform/     环境配置和数据库迁移
  migrations/             带摘要校验的 SQL 迁移
deploy/                  Compose、Dockerfile、宿主 Nginx 示例
script/                  开发、生成、校验、Runner 预检和冷备恢复
```

PostgreSQL 保存权威业务状态，`jobs` 表提供事务 Outbox 和租约队列。Redis 用于登录限流；开发时允许不用 Redis。持久数据集中在 `deploy/data`。项目不需要 MinIO，代码快照和运行证据保存在 `deploy/data/artifacts`。

## 本地开发

要求 Go 1.26、Node.js 24 或 26、PostgreSQL 17、Python 3.9+。Kiota 客户端已提交到源码目录，日常启动不需要安装 .NET。

```sh
make init
make bootstrap
```

`make init` 创建 `deploy/.env` 和 `deploy/data/secrets/`，已有配置不会覆盖。管理员初始密码在 `deploy/data/secrets/bootstrap_password`，不要提交或粘贴到日志。默认本地管理员邮箱由 `BOOTSTRAP_EMAIL` 设置。

数据库可选：

1. 使用现有开发 PostgreSQL：在 `deploy/.env` 设置 `DEV_DATABASE_URL`，数据库账号需有建表和 `pg_trgm` 扩展权限。
2. 使用本项目 Docker 开发数据库：运行 `make dev-services`，仅把数据库映射到 `127.0.0.1:55432`。`make init` 生成的开发连接文件已使用这个端口。

```sh
make dev
```

访问 <http://localhost:3000>。脚本先迁移，再启动 API、Worker 和 Next.js；结束脚本会停止这三个进程。配置文件按键读取，不执行其中的 Shell 内容。开发脚本支持 macOS/Linux，Windows 使用 WSL2；执行仓库代码需要单独的 Linux Runner。

本地开发固定使用 Web `3000`、API `8080` 和内部许可端口 `8081`。`deploy/.env` 的 `WEB_PORT/API_PORT` 仅调整 Compose 的宿主端口；修改它们时也要同步宿主 Nginx 的 `proxy_pass`。

## Docker 部署

普通用户运行 `make init`，在 `deploy/.env` 设置域名与 OIDC；把密钥填入对应的 `deploy/data/secrets/*` 文件。随后：

```sh
make up
```

Compose 启动 PostgreSQL、Redis、一次性迁移、API、Worker、Web。API 和 Worker 等待迁移成功。Nginx 安装在宿主机，参考 [nginx.example.conf](deploy/nginx.example.conf) 配置 HTTPS。容器的 Web/API 端口只绑定宿主回环地址，内部 Runner 许可端口不对公网开放。

首次上线、机器人和代码仓库配置、Runner 镜像及迁移方法见 [运行手册](docs/operations.md)。

当前开发机的访问地址、证书、运行状态和启停命令见 [开发机部署说明](docs/development-deployment.md)。构建镜像时可在 `deploy/.env` 设置 `GOPROXY`，默认使用 Go 官方模块源并保留校验和验证。

## 配置模型与执行器

管理员在“设置 → 模型接口”配置启用状态、API 地址、模型名称、API 密钥、每日 Token 上限和接口兼容选项。地址应是 `/chat/completions` 的父路径，例如供应商兼容接口的 `/v1`；模型需支持工具调用、结构化 JSON，并返回 Token 用量。配置保存在 PostgreSQL，密钥加密保存且不返回浏览器；编辑时留空保留，更换 API 地址需重新填写或明确清除密钥。

“设置 → 审批策略”统一配置创建开发任务、发送回复、发布知识及开发步骤的人工审批、自动批准和禁止执行。任务创建需要指定渠道、仓库、负责人、条件和路径；建议与执行记录进入同一个审批列表。开发步骤默认按策略审批、未匹配时人工处理；可改为全部人工或未匹配时自动批准，明确的人工、禁止和额度限制优先。

“设置 → 任务执行”管理任务网络、根文件系统、测试目录写入、资源配额与执行额度。时限 `0` 表示不设执行超时，暂停或撤权仍会停止任务。管理员保存后无需重启；未开始步骤的旧审批会失效，已开始执行保留原配置快照。

“设置 → Runtime Machines”用于登记可接入的执行宿主，包含执行地址、回调地址、调度权重、状态和镜像白名单。系统会从启用且就绪的机器中选择执行宿主，并把选择结果冻结到本次 attempt；attempt 一旦被某台机器接受，不会静默切换到其他机器。Runner Controller 必须使用已登记 Runtime Machine 的 ID 和令牌接入。

“文件与模型用量”中的代码快照、代码文件数、产物、工具调用和模型 Token 五项默认填 `0`，表示不设置单次任务额度；需要限制时可填写正整数。Context Budget 使用固定离线 `o200k_base` tokenizer 估算，而不是按字节长度；大 diff、测试日志和工具日志会在执行上下文中改为引用，由 Runner Agent 按需读取。实际可处理量仍取决于宿主可用磁盘、内存和模型服务能力；模型接口中配置的每日 Token 总额度独立生效。

任务默认不联网、根文件系统只读、测试目录只读。允许联网时使用专用 Rootless Docker 的 bridge 网络，可访问其网络可达地址，实际连通性由宿主路由决定。允许测试写入时，临时副本使用专用运行盘中的匿名 Docker 卷，新增依赖和构建文件不进入代码产物或 PR；修改已有源码或锁文件会使测试失败。代码快照与代码文件数配置正值时约束代码输入和快照，整个测试卷受专用运行盘总容量约束，新增文件不计入这两项额度。根文件系统可写时仍使用非 root 用户，目录权限继续生效。

开发 Agent 集成在 Runner Controller 内，使用 Eino v0.9.22 的 `flow/agent/react` 管理模型与工具循环、会话消息历史和工具分发，无需额外部署服务。模型请求通过 OpenAI 兼容适配器发送，使用本次执行冻结的配置。Controller 按阶段限制工具与文件路径，强制工具串行执行，并校验运行许可、预算和最终产物证据；执行上下文分为短 header 与只读 context 引用，Agent 可通过受限工具读取 manifest 和引用正文。

测试由 Controller 在沙箱中执行管理员配置且经过审批的固定命令；阶段推进、审批和发布由 Worker 管理。模型与发布凭据、Docker socket、数据库访问和 GitHub 发布权限都不进入任务沙箱。

网页资源模式默认 `required`，要求 cgroup v2 与可用的 CPU、内存和 PID 限制。仅可信开发环境可显式选择 `disabled`，同时执行固定为 1 个任务；此时任务没有这三项硬上限，可能耗尽宿主资源。每次执行始终使用独立容器、专用 rootless daemon、非 root 用户和运行许可校验，容器不挂载 Docker socket，支持暂停和停止。Runtime Machine 地址、服务令牌、回调地址、镜像白名单和宿主磁盘配置属于部署项，初始化与预检方法见 [模型与任务执行](docs/operations.md#5-模型与任务执行)。

## 检查与接口生成

```sh
# PostgreSQL 测试会在指定测试库内创建、删除独立 schema，不操作业务 schema。
export AGENTOWL_TEST_DATABASE_URL='postgres://test-user:test-password@127.0.0.1:5432/agentowl_test?sslmode=disable'
make check
```

未提供测试数据库时，相应集成测试会明确 Skip；本地缺少 Docker Compose 时会提示部署配置校验未运行。`make ci` 强制要求测试数据库、Docker Compose 和 Kiota，并检查 Go race、构建、前端、Compose 和客户端漂移。本地也可用下面的 `script/gen-client.sh --check` 单独检查生成代码漂移。

改动 API 契约后，使用 .NET 8+ 安装固定版本 Kiota 并重新生成：

```sh
dotnet tool install Microsoft.OpenApi.Kiota --version 1.36.1 --tool-path .cache/tools
make gen-client
script/gen-client.sh --check
```

本地 API 与 Worker 启动后，可运行 [HTTP 冒烟脚本](script/smoke-test.py)。它只允许回环地址，并创建标记为测试的记录：

```sh
AGENTOWL_SMOKE_EMAIL=admin@localhost \
AGENTOWL_SMOKE_PASSWORD_FILE="$PWD/deploy/data/secrets/bootstrap_password" \
python3 script/smoke-test.py
```

## 文档

- [需求基线](docs/requirements.md)
- [技术方案](docs/technical-design.md)
- [持续任务与自动接单](docs/task-runtime-design.md) · [实现与升级验收](docs/task-runtime-implementation.md)
- [部署、接入和故障处理](docs/operations.md)
- [验证结果与待验收项](docs/verification.md)
- [外部协议适配说明](backend/internal/integration/README.md)
