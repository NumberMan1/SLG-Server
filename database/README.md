# 数据库 SQL

本目录存放 MySQL 8.4 的建表和数据迁移脚本，路径为 `服务名/表名/版本号.sql`。表目录使用 MySQL 中的实际表名，版本号在各表目录内从 `0001` 开始递增。修改已迁移的表时新增下一个版本文件。

[`login-server/slg_accounts/0001.sql`](login-server/slg_accounts/0001.sql) 建立登录服务的 `slg_accounts` 表。账号以 UTF-8 字节精确匹配，有唯一约束。密码存为带盐的 bcrypt 哈希。MySQL 数据库和用户由部署环境提供。对空库或现有开发库执行迁移后，用登录服务的 `create-account` 命令建账号。

```sh
go run ./cmd/login-server migrate
SLG_ACCOUNT=demo SLG_ACCOUNT_PASSWORD="$SLG_DEMO_PASSWORD" go run ./cmd/login-server create-account
```

两个命令均读取 `SLG_MYSQL_DSN`。开发脚本会运行迁移，并在配置联调密码时建账号。以相同账号和密码重试是安全的。账号最长 64 字节，密码最长 72 字节。
