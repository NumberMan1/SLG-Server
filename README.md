# 服务端

目录参考服务端工程中 `cmd`、`internal`、`infrastructure`、`testkit` 和 `test-cases` 的职责划分。Go 和 C++ 使用各自的构建系统。

| 路径 | 职责 |
| --- | --- |
| `cmd/gateway`、`cmd/game-server` | Go 进程入口与启动配置 |
| `internal/gateway`、`internal/gameserver` | 网关路由与游戏服 HTTP 接口 |
| `infrastructure/httpserver` | Orbit 生命周期装配、进程信号和健康响应 |
| `infrastructure/logging` | NumberMan1/log 进程日志初始化和关闭 |
| `battle-server` | C++ 战斗服可执行目标与 CTest 冒烟用例 |
| [database](database/README.md) | MySQL 建表与数据迁移 SQL |
| `testkit` | 通过真实 HTTP 连接装配 Go 网关与游戏服 |
| `test-cases` | 通过网关访问游戏服和处理上游不可达的集成用例 |
| `scripts/verify.sh` | Go 测试与静态检查、C++ 构建与测试 |
| `scripts/dev.sh` | 在 Podman 中启动、查看和停止 SLG |
| [docs/architecture.md](docs/architecture.md) | 服务边界、存储和中间件选型 |
| [docs/client-protocol.md](docs/client-protocol.md) | HTTP 登录、TCP 认证和后续同步协议 |
| [proto](proto) | 双端 Protobuf 定义与 Go 生成代码 |

## 在 Podman 中启动

联调需要 Podman、Go 1.27.2、`curl` 和 POSIX `sh`。Windows 可使用 Git Bash，并先启动自己的 Podman 虚拟机。脚本用宿主机 Go 工具链编译 Linux 网关和游戏服，再用 Podman 构建镜像。

Orbit、Numbox 和 NumberMan1/log 通过 Go Modules 下载，版本和校验和固定在 `go.mod` 与 `go.sum`。服务端模块路径为 `github.com/NumberMan1/slg-server`。

当前 Orbit 需要 GitHub 仓库读取权限。新机器上先配置 GitHub 的 Git 凭据，再将 Orbit 加入 `GOPRIVATE`。如果已配置其他私有模块，请保留原有条目并以逗号追加。

```sh
export GOPRIVATE="${GOPRIVATE:+$GOPRIVATE,}github.com/NumberMan1/orbit"
```

在服务端仓库根目录把 `.env.example` 复制为 `.env`，编辑其中的 `SLG_DEMO_PASSWORD` 后启动。脚本以 shell 语法载入该文件，使用 `KEY=value` 格式，包含空格的值用引号包住。文件中的显式赋值优先于传入的同名变量。`.env` 由 Git 忽略。

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

已有 `gm-dev` 环境时，可把上述前三项都设为 `gm-dev`。网关只向宿主机回环地址公开 HTTP 和 TCP 端口，游戏服的 `8081` 在容器网络内使用。Unity 默认用 `demo` 和配置中的密码登录。修改 HTTP 端口后，在 Unity 登录场景填写新的网关地址。未设置密码时，登录接口返回 `503`。

使用以下命令检查网关到游戏服的链路，预期返回 `service = game-server` 和 `status = ok`。联调结束后可执行 `down` 停止并删除 SLG 容器。

```sh
curl http://127.0.0.1:8080/v1/game/health
sh scripts/dev.sh down
```

登录联调使用网关和游戏服，账号和一次性票据保存在网关内存中。C++20 战斗服目前用 CMake 和 CTest 验证启动。

Go 网关和游戏服通过 NumberMan1/log 把结构化 JSON 写到标准输出，日志用 `service` 标识进程。Podman 联调时可用 `podman logs slg-dev-gateway` 或 `podman logs slg-dev-game-server` 查看。

## 在 VS Code 中调试

在服务端仓库根目录运行 `code .`，安装 Go 和 C/C++ 扩展。以下调试选项来自本机的 `.vscode/`，该目录由 Git 忽略。首次调试 Go 时，可运行 **Go: Install/Update Tools** 安装 `dlv`。从已设置 `GOPRIVATE` 的终端启动 VS Code，让扩展和任务一并使用该设置。网关调试从 `.env` 读取演示账号和密码。

- 在 **Run and Debug** 选 **Go: Gateway + Game Server**，按 **F5** 同时启动两个本机进程。Unity 联调时填写 `http://127.0.0.1:28080`，网关通过登录响应返回 TCP 端口 `29090`，游戏服监听 `28081`。单独调试可选 **Go: Gateway** 或 **Go: Game Server**。
- 用 **Go: Integration Tests** 在 `test-cases` 中打断点。通过 **Tasks: Run Task** 运行 **Go: Test All** 或 **SLG: Verify Server**。
- 用 **C++: Battle Server Healthcheck** 调试 `--healthcheck`。启动前任务用 CMake 和 Ninja 构建 Debug 版本。Windows 和 Linux 需要 PATH 中的 GDB，macOS 需要 LLDB-MI。
- **Podman: Start SLG**、**Podman: Status** 和 **Podman: Stop SLG** 调用 `scripts/dev.sh`，沿用 `.env` 的 `gm-dev` 配置和默认 HTTP 端口 `8080`。Windows 任务使用 Git for Windows 自带的 `sh`。

## 运行校验

在服务端仓库根目录执行。战斗服使用 CMake 3.20 或更新版本和支持 C++20 的编译器。有 Ninja 时脚本默认使用 Ninja，也可以通过 `SLG_CMAKE_GENERATOR` 指定生成器。同一构建目录复用已配置的生成器。需要切换时用 `SLG_BUILD_DIR` 指定另一个目录。

```sh
sh scripts/verify.sh
```

只检查 Go 时，可执行 `go test ./...` 和 `go vet ./...`。只构建和检查 C++ 时，可执行以下命令，按工具链选择生成器。

```powershell
cmake -S battle-server -B .build/battle -G Ninja
cmake --build .build/battle --config Debug
ctest --test-dir .build/battle -C Debug --output-on-failure
```

Ninja 单配置构建在 Windows 上可运行 `./.build/battle/battle-server.exe --healthcheck`。多配置生成器将可执行文件放在对应配置目录中。

## GitHub Actions

Server CI 在 `develop`、`master` 的推送和目标为这两个分支的同仓 PR 上运行，也支持手动运行。它按 `go.mod` 安装 Go，下载并校验依赖，执行 `sh scripts/verify.sh`，再构建两个 Go 进程。HTTP 登录、TCP 认证和游戏服健康检查由现有集成用例覆盖，使用 runner 的回环端口。

在 SLG-Server 的 **Settings → Secrets and variables → Actions** 添加 `ORBIT_READ_TOKEN`。使用仅对 `NumberMan1/orbit` 有 **Contents: Read-only** 权限的细粒度 GitHub token。来自 fork 或 Dependabot 的 PR 跳过该任务。[GitHub 凭据配置](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
