# AgentOwl

AgentOwl 是一个面向研发团队的技术支持与开发协作工作台。它能把群聊消息、GitHub Issue 和人工录入的反馈统一归集为问题，基于审核过的知识库生成答复；遇到需要改代码的情况时，交由开发者确认目标，并在审批策略的约束下完成方案制定、编码、测试、审查和提 PR。

## 核心能力

- **飞书 / Lark 接入**：支持应用机器人 HTTP 回调和长连接两种方式接收群消息与私聊；内置加密验签、渠道白名单、历史与话题补采、消息编辑/撤回处理，以及交互卡片和飞书账号绑定。群聊还支持手动或定时历史同步。
- **GitHub 接入**：支持 Webhook 和无公网入口的定时增量轮询，同步 Issue、评论和 PR 状态。开发完成后基于锁定快照创建 Draft PR；后续根据 Review 或 CI 意见继续修改时，仍会走完整审批流程并更新同一个 PR。
- **知识库与自动答疑**：接入 OpenAI 兼容模型接口，支持范围分类、带引用的回答、证据不足转人工、模块负责人分派与超时提醒；支持将问题中的授权图片作为多模态输入。知识条目具备草稿、版本、审批发布、撤回和渠道可见范围，支持同步仓库 Markdown 与飞书文档，也可把已发送的优质问答整理成知识草稿。
- **开发流程与审批**：开发任务按 `制定方案 → 修改代码 → 运行测试 → 代码审查 → 提交 PR → 跟踪 PR` 推进。创建任务、发送回复、发布知识和各个开发阶段共用一套审批策略，支持人工审批、自动批准和禁止执行，任务随时可以暂停、恢复、取消或修改目标。
- **权限、审计与运维**：支持本地账号、OIDC 和飞书登录，提供按角色与资源范围的细粒度授权、完整操作记录、后台失败任务重试与忽略、外部写入结果核对，以及带权限和摘要校验的产物下载。

> 上线真实环境前，需要完成平台凭据、群权限、模型服务与 Linux 运行机器的配置。未配置模型时系统会自动转交人工；未就绪运行机器时，开发任务页会直接提示原因。各项功能的测试覆盖与待验收清单见 [验证记录](docs/verification.md)。

## 目录结构

```text
Frontend/                 Next.js、React、TypeScript、Tailwind CSS 4、Radix UI、SWR、Zustand
  app/                    工作台与设置页面
  components/             业务与基础 UI 组件
  lib/api/generated/      Kiota 生成的 TypeScript API 客户端
backend/
  api/openapi.yaml        OpenAPI 3.1 接口定义
  cmd/                    api、worker、runner-controller、migrate 四个进程入口
  internal/service/       业务服务、事务、审批、队列、知识库与发布逻辑
  internal/httpapi/       HTTP 路由、会话认证、CSRF 与内部运行许可接口
  internal/integration/   飞书/Lark、GitHub、模型接口适配与凭据加密
  internal/runner/        沙箱执行协议、Rootless Docker 控制与内置 Eino Agent
  internal/platform/      配置加载与数据库迁移
  migrations/             带摘要校验的 SQL 迁移脚本
deploy/                   Docker Compose、Dockerfile 与宿主机 Nginx 配置示例
script/                   本地开发、客户端生成、检查、运行机器预检与备份恢复脚本
```

核心业务数据存放在 PostgreSQL，`jobs` 表同时承担事务 Outbox 和带租约的异步任务队列；Redis 仅用于登录限流（本地开发可不启动）。所有持久化数据集中在 `deploy/data`，代码快照和执行产物直接落在 `deploy/data/artifacts`，无需额外部署 MinIO 等对象存储。

## 本地开发

环境要求：Go 1.26、Node.js 24 或 26、PostgreSQL 17、Python 3.9+。Kiota 生成的客户端代码已随仓库提交，日常开发无需安装 .NET。

### 1. 初始化配置与密钥

```sh
make init
make bootstrap
```

`make init` 会生成 `deploy/.env` 和 `deploy/data/secrets/` 下的初始密钥（已有文件不会覆盖）。管理员初始密码保存在 `deploy/data/secrets/bootstrap_password`，请勿提交或打印到日志；默认管理员邮箱通过 `BOOTSTRAP_EMAIL` 配置。

### 2. 准备数据库（二选一）

1. **使用已有 PostgreSQL**：在 `deploy/.env` 中配置 `DEV_DATABASE_URL`，数据库账号需具备建表和启用 `pg_trgm` 扩展的权限。
2. **使用内置开发库**：运行 `make dev-services`，通过 Docker 启动本地库并只绑定到 `127.0.0.1:55432`（`make init` 默认已指向该端口）。

### 3. 启动服务

```sh
make dev
```

启动后访问 <http://localhost:3000>。脚本会自动执行数据库迁移，并拉起 API、Worker 和前端开发服务；退出时一并清理三个进程。开发脚本支持 macOS 与 Linux（Windows 请使用 WSL2）；如需实际跑通仓库代码执行，需要单独配置 Linux 运行机器。

> 本地开发固定占用 Web `3000`、API `8080` 和内部许可端口 `8081`。`deploy/.env` 里的 `WEB_PORT` / `API_PORT` 仅用于 Docker Compose 映射；如做调整，记得同步修改宿主机 Nginx 的 `proxy_pass`。

## Docker 部署

在目标主机执行 `make init`，按需修改 `deploy/.env` 中的域名和 OIDC 配置，并将所需密钥写入 `deploy/data/secrets/*`，然后启动服务：

```sh
make up
```

Compose 会依次启动 PostgreSQL、Redis、一次性迁移任务、API、Worker 和 Web（API 与 Worker 会等待迁移完成后启动）。反向代理和 HTTPS 由宿主机 Nginx 承担，可参考 [nginx.example.conf](deploy/nginx.example.conf)。容器的 Web/API 端口默认仅绑定在回环地址，内部运行许可端口不对外暴露。

完整的首次上线步骤、机器人与仓库接入、运行机器部署及冷备份迁移指南，请查阅 [运维手册](docs/operations.md)。当前测试开发机的环境信息与启停命令见 [开发机部署说明](docs/development-deployment.md)。构建镜像时如需国内加速，可在 `deploy/.env` 中配置 `GOPROXY`。

## 模型、审批与任务执行

日常运行策略和模型接入都在 Web 端集中配置，保存后立即对新任务生效，无需重启服务：

- **模型接口**（`设置 → 模型接口`）：配置 OpenAI 兼容地址（填写到 `/chat/completions` 上一级，如 `.../v1`）、模型名、API 密钥、每日 Token 总额度及兼容选项。模型需支持工具调用、结构化 JSON 输出并返回 Token 用量。密钥在库中加密保存且不会回传前端；编辑时留空保持不变，更换地址时需重新填写或勾选清除。
- **审批策略**（`设置 → 审批策略`）：统一管理任务创建、回复发送、知识发布以及各开发步骤的规则（人工审批 / 自动批准 / 禁止执行），未命中任何规则时默认走人工审批。优先级遵循“禁止 > 人工 > 自动”，每条策略可单独开关审批通知。
- **任务执行**（`设置 → 任务执行`）：管理沙箱网络、文件系统权限、资源限额、执行超时与重试策略。
  - **沙箱隔离**：默认断网、根文件系统只读、测试目录只读，所有任务均在独立容器、专用 Rootless Docker 与非 root 用户下运行，容器内不注入模型密钥、发布凭据或 Docker Socket。
  - **联网与可写测试**：开启联网后接入专用 Rootless Docker 的 bridge 网络；允许测试写入时，会在独立匿名卷的临时副本中安装依赖并运行测试，新增文件不进入 PR，若改动已有源码或锁文件则测试直接判失败。
  - **超时与用量上限**：超时填 `0` 表示不限时（暂停或撤销许可仍会立即终止任务）。用量上限中的代码快照、文件数、产物、工具调用次数和单次模型 Token 默认均为 `0`（不设单次上限），实际容量仍受宿主机磁盘、内存以及每日模型总额度约束。
  - **资源限制模式**：默认 `required`，要求宿主机支持 cgroup v2 及 CPU/内存/PID 限额；仅在无 cgroup v2 的内部开发环境可改为 `disabled`（此时并发固定为 1，任务无硬件资源硬上限）。
- **运行机器**（`设置 → 运行机器`）：登记可调度的 Linux 执行宿主（执行地址、回调地址、权重、状态与镜像白名单）。系统从就绪机器中挑选宿主并绑定到本次执行，执行开始后不会中途切换机器。内置的开发 Agent 基于 Eino（`flow/agent/react`）运行在 Runner Controller 内，按阶段限制工具和文件路径，采用离线 `o200k_base` 分词估算上下文预算，大 diff 与日志按引用分页读取。

详细的宿主初始化脚本与预检命令见 [运维手册 · 模型与任务执行](docs/operations.md#5-模型与任务执行)。

## 测试、检查与客户端生成

```sh
# 集成测试会在指定测试库中临时建/删独立 schema，不会影响业务库
export AGENTOWL_TEST_DATABASE_URL='postgres://test-user:test-password@127.0.0.1:5432/agentowl_test?sslmode=disable'
make check
```

未设置测试数据库时，数据库相关集成测试会自动 Skip；本地缺少 Docker Compose 时会跳过部署文件检查。`make ci` 则强制要求测试库、Compose 和 Kiota 全部就位，执行完整的 Go race 测试、构建、前端校验与生成代码漂移检查。

修改 `backend/api/openapi.yaml` 后，需用固定版本的 Kiota（需要 .NET 8+）重新生成前端客户端并核对无漂移：

```sh
dotnet tool install Microsoft.OpenApi.Kiota --version 1.36.1 --tool-path .cache/tools
make gen-client
script/gen-client.sh --check
```

本地启动 API 与 Worker 后，可运行 [冒烟测试脚本](script/smoke-test.py) 快速验证基本链路（脚本仅允许连接回环地址，并会创建带测试标记的数据）：

```sh
AGENTOWL_SMOKE_EMAIL=admin@localhost \
AGENTOWL_SMOKE_PASSWORD_FILE="$PWD/deploy/data/secrets/bootstrap_password" \
python3 script/smoke-test.py
```

## 相关文档

- [需求文档](docs/requirements.md) — 产品定位、角色权限与功能需求基线
- [技术方案](docs/technical-design.md) — 系统架构、数据模型、审批与沙箱设计
- [持续任务与自动接单设计](docs/task-runtime-design.md) · [实现与发布记录](docs/task-runtime-implementation.md)
- [运维手册](docs/operations.md) — 生产部署、平台接入、运行机器初始化与备份迁移
- [开发机部署说明](docs/development-deployment.md) — 当前测试环境信息与常用运维命令
- [验证记录](docs/verification.md) — 测试覆盖范围与上线前待实测项
- [外部协议适配说明](backend/internal/integration/README.md) — 飞书/Lark、GitHub 与模型适配层约定
