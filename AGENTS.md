# SLG 服务端

- 日常开发在 `develop`，`master` 用于稳定版本。
- Go 进程入口放在 `cmd`，业务实现放在 `internal`，运行时装配放在 `infrastructure`。C++20 战斗服由 `battle-server` 的 CMake 构建。
- SLG 通过 Go Modules 使用固定版本的 Orbit、Numbox 和 NumberMan1/log。通用缺陷在对应上游库修复和验证，发布后更新 SLG 的依赖版本。
- 本机服务联调入口为 `sh scripts/dev.sh up`。从 `.env.example` 复制一份本机 `.env`，已有 `gm-dev` 环境可在其中指定 Podman 连接和网络。
- Go 业务单测随代码放置，集成用例放在 `test-cases`，共用测试装配放在 `testkit`。服务端校验入口为 `sh scripts/verify.sh`。
- 推送后按提交 SHA 跟踪 GitHub Actions 的 **Server CI**，等待 `verify` 作业结束。失败时查看失败步骤的日志，修复后核对新提交的运行。交付时附上运行链接和结果，如实说明跳过、取消或无法读取日志的情况。
