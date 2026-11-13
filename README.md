# 服务端

目录参考服务端工程中 `cmd`、`internal`、`infrastructure`、`testkit` 和 `test-cases` 的职责划分。Go 和 C++ 使用各自的构建系统。

| 路径 | 职责 |
| --- | --- |
| `cmd/gateway`、`cmd/game-server` | Go 进程入口与启动配置 |
| `internal/gateway`、`internal/gameserver` | 网关路由与游戏服 HTTP 接口 |
| `infrastructure/httpserver` | Orbit 生命周期装配、进程信号和健康响应 |
| `battle-server` | C++ 战斗服可执行目标与 CTest 冒烟用例 |
| `testkit` | 通过真实 HTTP 连接装配 Go 网关与游戏服 |
| `test-cases` | 通过网关访问游戏服和处理上游不可达的集成用例 |
| `scripts/verify.ps1` | Go 测试与静态检查、C++ 构建与测试 |
| [docs/architecture.md](docs/architecture.md) | 服务边界、存储和中间件选型 |

## 启动 Go 服务

使用 Go 1.27.2。SLG 通过 `go.mod` 的本地 `replace` 读取 `D:/go/src/github.com/NumberMan1/orbit` 和 `D:/go/src/github.com/NumberMan1/numbox`，因此这两个目录下的源码修改会直接进入 SLG 构建。换机器时在服务端仓库根目录执行以下命令，将示例路径换成各自的实际检出目录。首次构建可能需要下载其余 Go 依赖。

```powershell
go mod edit '-replace=github.com/NumberMan1/orbit=C:/src/orbit'
go mod edit '-replace=github.com/NumberMan1/numbox=C:/src/numbox'
```

在两个终端中分别进入服务端仓库根目录后运行以下命令。

```powershell
go run ./cmd/game-server
```

```powershell
go run ./cmd/gateway
```

| 环境变量 | 默认值 | 所属进程 |
| --- | --- | --- |
| `SLG_GAME_ADDR` | `127.0.0.1:8081` | 游戏服监听地址 |
| `SLG_GATEWAY_ADDR` | `127.0.0.1:8080` | 网关监听地址 |
| `SLG_GAME_URL` | `http://127.0.0.1:8081` | 网关使用的游戏服 HTTP(S) 基址 |

两个进程各自提供 `GET /healthz`。使用以下命令检查整个链路，预期返回 `service = game-server` 和 `status = ok`。

```powershell
Invoke-RestMethod http://127.0.0.1:8080/v1/game/health
```

## 运行校验

在服务端仓库根目录执行。战斗服使用 CMake 3.20 或更新版本和支持 C++20 的编译器。有 Ninja 时脚本默认使用 Ninja，也可以通过 `-CMakeGenerator` 指定生成器。同一构建目录复用其已配置的生成器。需要切换时用 `-BuildDirectory` 指定另一个目录。

```powershell
pwsh -NoProfile -File scripts/verify.ps1
```

只检查 Go 时，可执行 `go test ./...` 和 `go vet ./...`。只构建和检查 C++ 时，可执行以下命令，按工具链选择生成器。

```powershell
cmake -S battle-server -B .build/battle -G Ninja
cmake --build .build/battle --config Debug
ctest --test-dir .build/battle -C Debug --output-on-failure
```

Ninja 单配置构建在 Windows 上可运行 `./.build/battle/battle-server.exe --healthcheck`。多配置生成器将可执行文件放在对应配置目录中。
