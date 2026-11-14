# SLG 服务端

- 日常开发在 `develop`，`master` 用于稳定版本。
- Go 进程入口放在 `cmd`，业务实现放在 `internal`，运行时装配放在 `infrastructure`。C++20 战斗服由 `battle-server` 的 CMake 构建。
- SLG 通过 Go Modules 使用固定版本的 Orbit 和 Numbox。通用缺陷在对应上游库修复和验证，发布后更新 SLG 的依赖版本。
- 本机服务联调入口为 `sh scripts/dev.sh up`。从 `.env.example` 复制一份本机 `.env`，已有 `gm-dev` 环境可在其中指定 Podman 连接和网络。
- Go 业务单测随代码放置，集成用例放在 `test-cases`，共用测试装配放在 `testkit`。服务端校验入口为 `sh scripts/verify.sh`。
