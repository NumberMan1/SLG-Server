// Package database 将服务端使用的数据库模式嵌入可执行文件。
package database

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"

	"github.com/go-sql-driver/mysql"
)

//go:embed login-server/slg_accounts/0001.sql
var createAccounts string

//go:embed game-server/slg_world_snapshots/0001.sql
var createWorldSnapshots string

//go:embed game-server/slg_world_orders/0001.sql
var createWorldOrders string

//go:embed game-server/slg_world_orders/0002.sql
var addWorldOrderTroopType string

//go:embed game-server/slg_world_streams/0001.sql
var createWorldStreams string

//go:embed game-server/slg_world_events/0001.sql
var createWorldEvents string

//go:embed game-server/slg_world_leases/0001.sql
var createWorldLeases string

//go:embed game-server/slg_world_previous_snapshots/0001.sql
var createPreviousWorldSnapshots string

//go:embed game-server/slg_world_snapshot_checksums/0001.sql
var createWorldSnapshotChecksums string

// Migrate 建立账号表和世界存储表，可对已有数据库重复执行。
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, schema := range []string{createAccounts, createWorldSnapshots, createWorldOrders, createWorldStreams,
		createWorldEvents, createWorldLeases, createPreviousWorldSnapshots, createWorldSnapshotChecksums} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			return err
		}
	}
	// MySQL 不提供 ADD COLUMN IF NOT EXISTS。重复启动或并行迁移时允许已存在的列。
	_, err := db.ExecContext(ctx, addWorldOrderTroopType)
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1060 {
		return nil
	}
	return err
}
