package database

import (
	"context"
	"database/sql"
	_ "embed"
)

//go:embed login-server/slg_accounts/0001.sql
var createAccounts string

func Migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, createAccounts)
	return err
}
