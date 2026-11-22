# 服务端

目录参考服务端工程中 `cmd`、`internal`、`infrastructure`、`testkit` 和 `test-cases` 的职责划分。Go 和 C++ 使用各自的构建系统。

| 路径 | 职责 |
| --- | --- |
| `cmd/gateway`、`cmd/login-server` | Go 进程入口与启动配置 |
| `internal/gateway`、`internal/loginserver` | 网关路由和账号认证 |
| `internal/game-server` | C++ 权威世界、地图状态、行军和战果提交 |
| `infrastructure/httpserver` | Orbit 生命周期装配、进程信号和健康响应 |
| `infrastructure/logging` | NumberMan1/log 进程日志初始化和关闭 |
| `infrastructure/storage`、`infrastructure/servicediscovery` | Orbit 的 MySQL、Redis 和 etcd 装配 |
| `internal/battle-server` | C++ Orbit 战斗服务、可重放战斗规则和 CTest 用例 |
| [database](database/README.md) | MySQL 建表与数据迁移 SQL |
| `testkit` | 集成用例使用的 HTTP 测试装配 |
| `test-cases` | 通过网关访问游戏服和处理上游不可达的集成用例 |
| `scripts/verify.sh` | Go 测试与静态检查、C++ 构建与测试 |
| `scripts/dev.sh` | 在 Podman 中启动、查看和停止 SLG |
| [docs/architecture.md](docs/architecture.md) | 服务边界、存储和中间件选型 |
| [docs/client-protocol.md](docs/client-protocol.md) | HTTP 登录、TCP 认证和后续同步协议 |
| [proto](proto) | 双端 Protobuf 定义与 Go 生成代码 |

## 在 Podman 中启动

联调需要 Podman、Go 1.27.2、CMake、`curl` 和 POSIX `sh`。Windows 可使用 Git Bash，并先启动自己的 Podman 虚拟机。脚本编译 Linux 网关和登录服，校验 C++ 依赖，再在 Podman 中构建游戏服和运行镜像。

Orbit、Numbox 和 NumberMan1/log 通过 Go Modules 下载，版本和校验和固定在 `go.mod` 与 `go.sum`。Go 服务和两个 C++ 服务共用 Orbit v0.4.1。C++ 服务从该模块目录链接 `orbit::cpp`，Podman 由宿主机在构建前暂存该源码。服务端模块路径为 `github.com/NumberMan1/slg-server`。

当前 Orbit 需要 GitHub 仓库读取权限。新机器上先配置 GitHub 的 Git 凭据，再将 Orbit 加入 `GOPRIVATE`。如果已配置其他私有模块，请保留原有条目并以逗号追加。

```sh
export GOPRIVATE="${GOPRIVATE:+$GOPRIVATE,}github.com/NumberMan1/orbit"
```

在服务端仓库根目录把 `.env.example` 复制为 `.env`，填写 MySQL DSN、Redis 密码和本机联调密码。先启动 MySQL、Redis 和 etcd，并确认它们可从 SLG 容器网络访问。脚本以 shell 语法载入该文件，使用 `KEY=value` 格式。包含括号等 shell 特殊字符的值要加引号。文件中的显式赋值优先于传入的同名变量。`.env` 由 Git 忽略。

```sh
cp .env.example .env
sh scripts/dev.sh up
sh scripts/dev.sh status
```

也可以通过 `SLG_ENV_FILE` 选择自己维护的另一份本机配置。

| 配置 | 默认 | 用途 |
| --- | --- | --- |
| `SLG_PODMAN_MACHINE` | 空 | 可选，启动指定的现有 Podman 虚拟机 |
| `SLG_PODMAN_CONNECTION` | Podman 当前连接 | 可选，指定 Podman 连接 |
| `SLG_PODMAN_NETWORK` | `slg-dev` | 默认自动创建，指定其他网络时复用已有网络 |
| `SLG_GATEWAY_HTTP_PORT` | `8080` | Unity 登录和健康检查的宿主机端口 |
| `SLG_GATEWAY_TCP_PORT` | `19090` | Unity 从登录响应获得的 TCP 端口 |
| `SLG_WEB_ROOT` | 空 | 直接运行网关时可填 Unity Web 构建目录，在同一 HTTP 站点提供试玩页面 |
| `SLG_MYSQL_DSN` | 登录服务必填 | MySQL 连接串，其中数据库需提前创建 |
| `SLG_MYSQL_ADDRESS` | 空 | 直接运行登录服务时可覆盖 MySQL TCP 地址，沿用连接串中的凭据和数据库 |
| `SLG_REDIS_MODE` | `cluster` | Redis 连接模式 |
| `SLG_REDIS_ADDRESSES` | Dev 脚本在 `gm-dev` 网络使用三个 `gm-redis-*` 节点，其他网络使用三个 `slg-dev-redis-*` 节点。直接运行网关使用 `127.0.0.1:6379` | Cluster 种子地址以逗号分隔，单机模式必须填写一个地址 |
| `SLG_REDIS_USERNAME`、`SLG_REDIS_PASSWORD` | 空 | Redis ACL 凭据 |
| `SLG_REDIS_DIAL_ADDRESSES` | 空 | 直接运行进程时可把 Redis Cluster 返回的节点地址映射到宿主机端口，格式为逗号分隔的 `节点地址=拨号地址` |
| `SLG_REDIS_KEY_PREFIX` | `slg:login` | 同环境登录服务和网关共享的票据前缀 |
| `SLG_ETCD_ENDPOINTS` | Dev 脚本使用 gm-dev 内部地址，直接运行进程使用 `http://127.0.0.1:2379` | 逗号分隔的 etcd 地址 |
| `SLG_ETCD_PREFIX` | `/slg/services` | 同环境服务共享的发现前缀 |
| `SLG_ETCD_USERNAME`、`SLG_ETCD_PASSWORD` | 空 | etcd 凭据 |
| `SLG_LOGIN_ADDR` | `127.0.0.1:8082` | 直接运行登录服务时的 HTTP 监听地址 |
| `SLG_LOGIN_CONCURRENCY` | `16` | 每个登录服实例同时处理的登录请求数，须为正整数 |
| `SLG_REGISTER_CONCURRENCY` | `4` | 每个登录服实例同时处理的注册请求数，须为正整数 |
| `SLG_REGISTER_RATE_PER_SECOND` | `0` | 每个登录服实例的可选注册请求速率。`0` 关闭每秒限额 |
| `SLG_LOGIN_ADVERTISE_URL` | `http://` + 登录服务监听地址 | 登录服务公布给网关的 HTTP 地址 |
| `SLG_LOGIN_INSTANCE_ID` | 进程启动生成 | 可选，指定实例 ID 时须在发现前缀下唯一 |
| `SLG_GAME_ADDR` | `127.0.0.1:8081` | 直接运行 C++ 游戏服时的内部 HTTP 监听地址 |
| `SLG_GAME_RPC_ADDR` | `127.0.0.1:8083` | 直接运行 C++ 游戏服时的 Orbit TCP 监听地址 |
| `SLG_GAME_UPSTREAM_URL` | `http://127.0.0.1:8081` | 网关调用游戏服健康接口的地址 |
| `SLG_GAME_RPC_UPSTREAM` | `127.0.0.1:8083` | 网关发送玩法请求的目标。Podman 使用游戏服容器的内网 DNS |
| `SLG_BATTLE_RPC_ADDR` | `127.0.0.1:8084` | 直接运行战斗服时的 Orbit TCP 监听地址 |
| `SLG_BATTLE_RPC_UPSTREAM` | `127.0.0.1:8084` | 游戏服发起战斗结算的目标。Podman 使用战斗服容器的内网 DNS |
| `SLG_WORLD_IDLE_SECONDS` | `60` | 游戏服回收不再请求世界的玩家名额前等待的秒数 |

已有 `gm-dev` 环境时，将前三个 Podman 配置项都设为 `gm-dev`。在本机的 `dev-services` 目录运行 `./services.ps1 redis-up`，再执行 `podman --connection gm-dev start gm-mysql gm-etcd-1 gm-etcd-2 gm-etcd-3`。在 SLG 的 `.env` 填入该集群的 `SLG_REDIS_PASSWORD`。脚本自动选用 `gm-redis-1:6379`、`gm-redis-2:6379` 和 `gm-redis-3:6379`。网关只向宿主机回环地址公开 HTTP 和 TCP 端口。登录服的 `8082`、游戏服的 HTTP `8081` 和 Orbit TCP `8083`、战斗服的 Orbit TCP `8084` 均在容器网络内使用。

使用独立的 `slg-dev` 网络时，在 `.env` 设置 `SLG_REDIS_MODE=cluster` 和 `SLG_REDIS_PASSWORD`，然后执行以下命令。

```sh
sh scripts/dev.sh redis-cluster-up
sh scripts/dev.sh up
```

这会在 Podman 网络上创建三个 Redis 主节点和各自的数据卷，并分配全部 16384 个槽位。节点名称为 `slg-dev-redis-1` 至 `slg-dev-redis-3`，端口 `6379` 仅供容器网络访问。该拓扑用于验证分片，三个节点均无副本。脚本重复运行时复用现有节点。应用的 `down` 只处理应用容器。接入其他 Redis Cluster 时，在 `SLG_REDIS_ADDRESSES` 填入种子地址，并确认网关可访问集群返回的各节点地址。

启动脚本会应用 [MySQL 迁移](database/README.md)。玩家可在 Unity 登录页用 **注册账号** 创建账号。填写 `SLG_DEMO_PASSWORD` 时，启动脚本也可以创建 `SLG_DEMO_ACCOUNT` 指定的联调账号，默认为 `demo`。修改 HTTP 端口后，在 Unity 登录场景填写新的网关地址。

启动脚本通过网关的 `/readyz` 检查登录服、MySQL、Redis 票据读写、Orbit 游戏连接和战斗结算连接。全部可用时返回 `204`。`/v1/game/health` 返回游戏服的存活状态。联调结束后可执行 `down` 停止并删除 SLG 容器。

```sh
curl -i http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/v1/game/health
sh scripts/dev.sh down
```

登录服务通过 MySQL 校验密码并把一次性票据写入 Redis，通过 etcd 公布给网关。认证通过后，两个账号分别取得出生地。Unity 可直接创建第二个账号，也可用 `SLG_DEMO_ACCOUNT` 和 `SLG_DEMO_PASSWORD` 配置联调账号。占领中立地块后可以攻打相邻敌方领地。战斗服计算双方存活兵力，游戏服提交战果，双方看到同一份战报。

Go 进程通过 NumberMan1/log 把结构化 JSON 写到标准输出，日志用 `service` 标识进程。Podman 联调时可用 `podman logs <容器名>` 查看，容器分别为 `slg-dev-gateway`、`slg-dev-login-server`、`slg-dev-game-server` 和 `slg-dev-battle-server`。

浏览器的长连接入口是网关 HTTP 端口上的 `/v1/session` WebSocket。在线站点使用 HTTPS 和 WSS，在反向代理处终止 TLS，并为静态页面、HTTP 登录和 WebSocket 配置同一域名。反向代理应保留原始 Host，并转发 WebSocket Upgrade。直接运行网关时可通过 `SLG_WEB_ROOT` 提供客户端未压缩 Web 构建。Podman 模式可由前置 Web 服务器提供这些静态文件。

## 在 VS Code 中调试

在服务端仓库根目录运行 `code .`，安装 Go 和 C/C++ 扩展。以下调试选项来自本机的 `.vscode/`，该目录由 Git 忽略。首次调试 Go 时，可运行 **Go: Install/Update Tools** 安装 `dlv`。从已设置 `GOPRIVATE` 的终端启动 VS Code，让扩展和任务一并使用该设置。

VS Code 的 Go 网关、登录服和 Podman 共用 `.env`。在 `.env` 明确填写 `SLG_REDIS_ADDRESSES=gm-redis-1:6379,gm-redis-2:6379,gm-redis-3:6379`，并按上文启动 `gm-dev` 的 MySQL、etcd 和 Redis。Go 启动项在 `launch.json` 的 `env` 中覆盖 `SLG_ETCD_ENDPOINTS` 为宿主机回环地址，并用独立的 `SLG_ETCD_PREFIX=/slg/local-debug/services` 隔离调试注册。登录服务通过 `SLG_MYSQL_ADDRESS=127.0.0.1:3306` 复用同一 MySQL 连接串。登录服务和网关通过 `SLG_REDIS_DIAL_ADDRESSES=gm-redis-1:6379=127.0.0.1:16301,gm-redis-2:6379=127.0.0.1:16302,gm-redis-3:6379=127.0.0.1:16303` 访问同一 Redis Cluster，调试票据使用 `SLG_REDIS_KEY_PREFIX=slg:local-debug:login`。

- 在 **Run and Debug** 选 **SLG Stack**，按 **F5** 启动四个本机进程。Unity 联调时填写 `http://127.0.0.1:28080`，网关通过登录响应返回 TCP 端口 `29090`。登录服监听 `28082`，游戏服使用 `28081` 探活和 `28083` 处理玩法，战斗服在 `28084` 处理内网结算。四个进程均可单独启动调试。
- 用 **Go: Integration Tests** 在 `test-cases` 中打断点。通过 **Tasks: Run Task** 运行 **Go: Test All** 或 **SLG: Verify Server**。
- 用 **C++: Battle Server Healthcheck** 调试 `--healthcheck`。启动前任务用 CMake 和 Ninja 构建 Debug 版本。Windows 和 Linux 需要 PATH 中的 GDB，macOS 需要 LLDB-MI。
- **Podman: Start SLG**、**Podman: Status** 和 **Podman: Stop SLG** 调用 `scripts/dev.sh`，沿用 `.env` 的 `gm-dev` 配置和默认 HTTP 端口 `8080`。Windows 任务使用 Git for Windows 自带的 `sh`。

## 运行校验

在服务端仓库根目录执行。两个 C++ 服务使用 CMake 3.20 或更新版本，并沿用 Orbit 的 C++23 要求。战斗规则库使用 C++20。有 Ninja 时脚本默认使用 Ninja，也可以通过 `SLG_CMAKE_GENERATOR` 指定生成器。脚本构建游戏服及战斗服，并用真实进程运行跨进程联通用例。可用 `SLG_GAME_BUILD_DIR` 和 `SLG_BUILD_DIR` 分别覆盖游戏服和战斗服的构建目录。

验证 Orbit 的 C++ 上游改动时，可把环境变量 `SLG_ORBIT_CPP_DIR` 设为本地 Orbit 的 `cpp` 目录（例如 `D:/go/src/github.com/NumberMan1/orbit/cpp`）。本机构建与 Podman 源码暂存均识别此变量。默认使用 `go.mod` 锁定的发布版本。

```sh
sh scripts/verify.sh
```

只检查 Go 时，可执行 `go test ./...` 和 `go vet ./...`。只构建和检查 C++ 时，可执行以下命令，按工具链选择生成器。

```powershell
cmake -S internal/game-server -B .build/internal/game -G Ninja -DCMAKE_BUILD_TYPE=Debug
cmake --build .build/internal/game --config Debug
cmake -S internal/battle-server -B .build/internal/battle -G Ninja
cmake --build .build/internal/battle --config Debug
ctest --test-dir .build/internal/battle -C Debug --output-on-failure
```

Ninja 单配置构建在 Windows 上可运行 `./.build/internal/battle/battle-server.exe --healthcheck`。多配置生成器将可执行文件放在对应配置目录中。

## GitHub Actions

Server CI 在 `develop`、`master` 的推送和目标为这两个分支的同仓 PR 上运行，也支持手动运行。它使用临时 MySQL、单机 Redis 和 etcd 验证登录转发、TCP 票据和服务发现，构建 C++ 游戏服和战斗服后验证共享地图、战斗结果和战斗服重连，并构建两个 Go 进程。

本地可在校验命令前提供 `SLG_TEST_MYSQL_DSN`、`SLG_TEST_REDIS_ADDRESSES`、`SLG_TEST_REDIS_PASSWORD` 和 `SLG_TEST_ETCD_ENDPOINTS`，复用真实中间件。Redis 集群测试额外设置 `SLG_TEST_REDIS_MODE=cluster`。登录中间件用例会应用建表迁移并删除本次创建的测试账号。

在 SLG-Server 的 **Settings → Secrets and variables → Actions** 添加 `ORBIT_READ_TOKEN`。使用仅对 `NumberMan1/orbit` 有 **Contents: Read-only** 权限的细粒度 GitHub token。来自 fork 或 Dependabot 的 PR 跳过该任务。[GitHub 凭据配置](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
