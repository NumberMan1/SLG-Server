# SLG 服务端

- 日常开发在 `develop`，`master` 用于稳定版本。
- Go 进程入口放在 `cmd`，业务实现放在 `internal`，运行时装配放在 `infrastructure`。C++20 战斗服由 `battle-server` 的 CMake 构建。
- SLG 使用 Orbit 和 Numbox 的本地源码。通用缺陷在对应库修复和验证，SLG 维护业务路由及应用装配。
- Go 业务单测随代码放置，集成用例放在 `test-cases`，共用测试装配放在 `testkit`。服务端校验入口为 `pwsh -NoProfile -File scripts/verify.ps1`。
