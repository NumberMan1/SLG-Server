// Package database 将服务端使用的数据库模式嵌入可执行文件。
package database

import (
	"context"
	"database/sql"
	_ "embed"
)

//go:embed login-server/slg_accounts/0001.sql
var createAccounts string

// Migrate 执行登录账号表的建表 SQL，可对已有数据库重复执行。
func Migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, createAccounts)
	return err
}
