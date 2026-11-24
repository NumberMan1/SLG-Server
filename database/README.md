# 数据库 SQL

本目录存放 MySQL 8.4 的建表和数据迁移脚本，路径为 `服务名/表名/版本号.sql`。表目录使用 MySQL 中的实际表名，版本号在各表目录内从 `0001` 开始递增。修改已迁移的表时新增下一个版本文件。

[`login-server/slg_accounts/0001.sql`](login-server/slg_accounts/0001.sql) 建立登录服务的 `slg_accounts` 表。账号以 UTF-8 字节精确匹配，有唯一约束。密码存为带盐的 bcrypt 哈希。MySQL 数据库和用户由部署环境提供。对空库或现有开发库执行迁移后，用登录服务的 `create-account` 命令建账号。

[`game-server/slg_world_snapshots/0001.sql`](game-server/slg_world_snapshots/0001.sql) 保存地图的最近一份完整快照及其最后覆盖的事件序号。快照包含领地、部队、进行中的战斗和最近的战报。

[`game-server/slg_world_streams/0001.sql`](game-server/slg_world_streams/0001.sql) 保存各张地图已提交的最后一个事件序号。[`game-server/slg_world_events/0001.sql`](game-server/slg_world_events/0001.sql) 按地图和事件序号保存状态变更。[`game-server/slg_world_previous_snapshots/0001.sql`](game-server/slg_world_previous_snapshots/0001.sql) 保留回退用的上一份快照，清理任务只删除它已覆盖的事件。

[`game-server/slg_world_leases/0001.sql`](game-server/slg_world_leases/0001.sql) 保存世界当前的写入者和到期时间。同一个世界的游戏服需要先取得租约再开放服务。

[`game-server/slg_world_snapshot_checksums/0001.sql`](game-server/slg_world_snapshot_checksums/0001.sql) 保存快照写入时的 SHA-256 校验和。旧快照内容通过校验后才能成为新的回退点。

[`game-server/slg_world_orders/0001.sql`](game-server/slg_world_orders/0001.sql) 建立指令去重表。游戏服在追加变更事件的同一事务内写入成功指令，按地图和席位版本保留记录。已有快照在首次启动时作为事件流起点，旧存档内嵌的指令同时迁入去重表。

```sh
go run ./cmd/login-server migrate
SLG_ACCOUNT=demo SLG_ACCOUNT_PASSWORD="$SLG_DEMO_PASSWORD" go run ./cmd/login-server create-account
```

两个命令均读取 `SLG_MYSQL_DSN`。`migrate` 建立账号、租约、地图快照及其校验和、事件流和指令去重表。首次升级到租约版本时，先停止使用旧版本的同世界游戏服，再启动新版本。开发脚本会运行迁移，并在配置联调密码时建账号。以相同账号和密码重试是安全的。账号最长 64 字节，密码最长 72 字节。
