# AgentOwl 开发机部署

记录日期：2026-10-29。

## 访问与环境

| 项目 | 当前配置 |
| --- | --- |
| 访问地址 | [https://10.37.90.168](https://10.37.90.168) |
| SSH | `wangruilong@10.37.90.168` |
| 项目目录 | `/data00/home/wangruilong/Services/AgentOwl` |
| 宿主 | veLinux 2、amd64、cgroup v1 |
| 容器工具 | Docker 29.9.2、Compose 5.6.1 |
| 入口 | 宿主 Nginx，转发到回环地址上的 Web/API |
| 管理员 | `admin@agentowl.local`，本地账号登录 |
| 持久数据 | 项目目录下的 `deploy/data` |
| 数据状态 | 使用全新数据库部署，未迁移本机数据 |
| Runner | 专用 rootless Docker；网页中关闭资源配额，并发 1 |
| 执行配置 | 版本 3；文件与模型用量五项均为 0（不设任务额度）；允许联网、允许测试写入临时副本、根文件系统只读；按策略审批，执行时限 900 秒 |

管理员密码仅保存在远端项目的 `deploy/data/secrets/bootstrap_password`，本文不记录密码值。

在自己的终端中读取初始密码：

```sh
ssh wangruilong@10.37.90.168 'cat ~/Services/AgentOwl/deploy/data/secrets/bootstrap_password'
```

HTTPS 使用有效期 30 天的开发自签证书。公钥证书位于 `deploy/agentowl-dev.crt`，可导入开发设备的证书信任列表；TLS 私钥位于 `deploy/data/secrets/tls`，不得分发。证书于 **2026-11-27 03:47:36 UTC** 到期，继续使用时请在到期前更新证书。

## 运行状态与范围

Web、API、Worker、PostgreSQL 和 Redis 已运行，数据库迁移已成功。后端运行镜像包含 Go 静态程序和 CA 证书，基于 `scratch`；GitHub 发布通过 API 执行，不依赖容器内的 Git 命令。

已完成实际 Linux 镜像构建、Compose 配置校验，以及 HTTPS 登录页和静态资源、Secure Cookie、CSRF、幂等、问题认领与反馈、知识审批、真实 Worker 发布、内部路径 404 和登出验证。API/Worker/Web 重启后，重新登录仍可读取同一条已解决问题和已发布知识；API 就绪检查与校验证书后的登录页请求正常。详情见 [验证记录](verification.md)。

10 月 29 日完成版本 2 的重启复验后，已部署取消五项任务配额的新版本，并将执行配置保存为版本 3；HTTPS 保存与复读均通过，API 与 Runner 读取结果一致。代码快照、文件数、产物、工具次数和单次任务 Token 均为 0，不再有原来的固定上限；每日模型总额度仍为已配置的 1000 万 Token，独立生效。四个应用容器均在运行，镜像与部署版本一致。本轮独立测试数据库、测试账号、临时测试镜像、远端临时目录与二进制，以及本机 12 个临时文件/目录已清理，业务数据和回滚备份保留。

模型地址、名称、密钥及每日用量在「设置 → 模型接口」中配置，由数据库保存；密钥加密存储，页面只显示是否已配置。开发机已在网页启用模型并配置密钥；本次只验证配置状态，没有调用真实模型供应商。真实模型回答、飞书/Lark 接入及 GitHub PR 发布尚未验收。实际代码仓库须配置包含语言和构建工具的工具链镜像，并将其固定摘要加入 `RUNNER_ALLOWED_IMAGES`；联网测试可安装项目依赖，离线执行则需预装依赖。目前预装的 Alpine 镜像用于运行环境验收。

Runner Controller 已启用，API 和 Worker 通过内部网络连接 `http://runner-controller:8090`。宿主使用 cgroup v1，在「设置 → 任务执行」中关闭 CPU、内存和进程配额，同时执行的任务数为 1；CPU、内存和进程数没有任务级硬上限，任务可能耗尽宿主资源。网络、根文件系统、测试目录写入、开发审批方式与执行时限均可在网页配置，保存后用于后续执行；已开始的执行保留原配置。独立容器、专用 rootless daemon、非 root 用户、磁盘边界和运行许可校验始终保留。默认 `required` 模式在该宿主上的拒绝行为已验证。配置步骤见 [运行手册](operations.md#5-runner-宿主)。

专用账号为 `agentowl-runner`（UID 2001、GID 2002），用户服务为 `agentowl-docker.service`。运行数据位于 `deploy/data/runner`：runtime 使用 8 GiB ext4 文件系统，任务工作区使用 1 GiB tmpfs；containerd 的持久数据也位于该 runtime 文件系统。初始化配置了宿主账号、用户服务和挂载，未切换宿主 cgroup 或重启主机。

真实 Runner 的基础 8 项验收已通过 bridge + 只读根目录中的文件读写与产物收集、离线隔离、超时停止、bridge 可写测试副本、已有源码修改拒绝、可写根文件系统，以及无执行超时时的手动停止和撤权停止。可选公网探针此前成功下载 npm 公开依赖，最后一次下载在 DNS 解析成功后超时；该探针不计入最终基础 8 项通过结果，开发机公网访问仍可能不稳定。可写测试使用本次执行的匿名 Docker 卷，工作目录为 `/workspace/source`；源码副本与审批快照核验一致，新增依赖不进入产物或 PR，卷使用专用 runtime 文件系统的总容量。模型与许可服务在该验收中使用测试替身，不代表真实业务开发流程已完成验收。验收任务、容器、临时卷和产物已清理。

本机 Web、API、Worker 和专用 PostgreSQL 已停止；本地测试数据库、临时备份、日志及构建缓存已清理。

## 常用命令

以下命令在开发机执行，包含已启用的 Runner：

```sh
ssh wangruilong@10.37.90.168
cd /data00/home/wangruilong/Services/AgentOwl

# 查看运行与迁移状态
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner ps -a

# 启动；源码或依赖变化后重新构建
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner up -d --build

# 查看应用日志
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner logs --tail=100 api worker web runner-controller

# 停止应用容器，保留数据
sudo docker compose --env-file deploy/.env -f deploy/compose.yaml --profile runner stop

# 使用开发证书检查 HTTPS 入口
curl --cacert deploy/agentowl-dev.crt --head https://10.37.90.168/
```

Compose 命令只管理应用容器。Runner 专用 Docker 由宿主用户服务管理，冷备前还需停止该服务并卸载 runtime/tasks，见运行手册。Nginx 是宿主服务，可用 `sudo nginx -t` 检查配置、`sudo systemctl status nginx` 查看状态。

## 构建与宿主备注

- Docker daemon 已配置镜像源 `docker.m.daocloud.io`；Go 构建通过 `GOPROXY` 构建参数配置，开发机 `deploy/.env` 使用 `goproxy.cn`。
- 宿主原有 `security-global-root-ca` 软件包的安装后脚本不识别 veLinux，仍处于未配置完成状态，可能使后续 apt 操作返回失败。Docker 和 Nginx 软件包已安装成功；该宿主 CA 包问题未作修改，不属于应用启动故障。
