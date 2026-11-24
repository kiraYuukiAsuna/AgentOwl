# AgentOwl 开发机部署说明

记录日期：2026-10-29

## 环境与访问

| 项目 | 当前配置 |
| --- | --- |
| 访问地址 | <https://10.37.90.168> |
| SSH | `wangruilong@10.37.90.168` |
| 项目目录 | `/data00/home/wangruilong/Services/AgentOwl` |
| 宿主机 | veLinux 2、amd64、cgroup v1 |
| 容器环境 | Docker 29.9.2、Compose 5.6.1 |
| 入口代理 | 宿主机 Nginx，反代至回环地址的 Web / API |
| 初始管理员 | `admin@agentowl.local`（本地账号登录） |
| 数据目录 | 项目下的 `deploy/data`（全新初始化库） |
| 运行机器 | 专用 Rootless Docker；网页端已关闭资源限制，并发为 1 |
| 执行配置 | 版本 3：五项用量上限均为 `0`（不限单次），允许联网，允许测试写入临时副本，根文件系统只读，执行时限 900 秒 |

管理员初始密码保存在远端项目的 `deploy/data/secrets/bootstrap_password`，可通过以下命令查看：

```sh
ssh wangruilong@10.37.90.168 'cat ~/Services/AgentOwl/deploy/data/secrets/bootstrap_password'
```

HTTPS 当前使用有效期 30 天的自签开发证书，将于 **2026-11-27 03:47:36 UTC** 过期，继续使用需提前续签。公钥位于 `deploy/agentowl-dev.crt`（可导入本地开发机信任列表），私钥位于 `deploy/data/secrets/tls`（切勿分发）。

## 当前运行状态

- **核心服务**：Web、API、Worker、PostgreSQL、Redis 与 Runner Controller 均正常运行，数据库迁移已完成。后端容器基于 `scratch`（仅含 Go 静态二进制与 CA 证书），GitHub 发布直接走 API，不依赖容器内 Git。
- **业务与配置**：HTTPS 登录、CSRF、幂等、问题处理、知识审批与 Worker 异步发布、以及容器重启后的数据恢复均已通过验证（详情见 [验证记录](verification.md)）。
- **模型配置**：已在「设置 → 模型接口」启用模型并配置密钥，每日上限 1000 万 Token；当前仅验证了配置持久化与加密存储，尚未联调真实模型供应商。
- **Runner 与隔离环境**：
  - API 和 Worker 通过内部网络访问 `http://runner-controller:8090`。
  - 专用运行账号为 `agentowl-runner`（UID `2001`、GID `2002`），systemd 用户服务为 `agentowl-docker.service`。数据位于 `deploy/data/runner`，其中 `runtime` 为 8 GiB ext4 文件系统（含 containerd 数据），任务工作区为 1 GiB tmpfs。
  - 由于宿主机使用 cgroup v1，网页端已将资源限制模式设为关闭（并发锁定为 1），意味着没有 CPU/内存/PID 硬上限，但独立容器、专用 Rootless daemon、非 root 用户、磁盘边界与运行许可校验仍全部生效。初始化与预检步骤见 [运维手册 · 运行机器宿主初始化](operations.md#52-运行机器宿主初始化)。
  - 真实容器环境的 8 项基础能力（bridge 联网、离线隔离、只读/可写根文件系统、临时测试卷与源码防篡改校验、超时与手动/撤权停止）已通过测试。当前预装的 Alpine 镜像仅用于环境自检，跑实际业务仓库前需替换为带对应编译工具链的镜像并加入 `RUNNER_ALLOWED_IMAGES`。另需注意开发机公网访问偶有波动，下载外部依赖可能超时。
- **待联调项**：真实模型问答与代码生成、飞书/Lark 线上群接入、GitHub 实际 PR 发布尚未验收。

## 常用运维命令

登录开发机并进入项目目录后可执行以下命令（已包含 `--profile runner`）：

```sh
ssh wangruilong@10.37.90.168
cd /data00/home/wangruilong/Services/AgentOwl

# 查看容器与迁移状态
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner ps -a

# 启动或更新代码后重新构建
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner up -d --build

# 查看最近 100 行应用日志
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner logs --tail=100 api worker web runner-controller

# 停止应用容器（保留数据）
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner stop

# 校验证书并探测 HTTPS 入口
curl --cacert deploy/agentowl-dev.crt --head https://10.37.90.168/
```

> 以上 Compose 命令只管理应用容器。Runner 专用 Docker 由宿主机 systemd 用户服务管理，做冷备份前需先停止该服务并卸载 `runtime` 与 `tasks`。宿主机 Nginx 可通过 `sudo nginx -t` 和 `sudo systemctl status nginx` 检查。

## 宿主机备注

- Docker daemon 已配置镜像源 `docker.m.daocloud.io`；Go 镜像构建通过 `deploy/.env` 中的 `GOPROXY=goproxy.cn` 加速。
- 宿主机原有的 `security-global-root-ca` 系统包因安装脚本不识别 veLinux 处于未配置状态，可能导致后续 `apt` 命令报错；Docker 和 Nginx 均已正常安装，该系统包问题未做改动，不影响应用运行。
