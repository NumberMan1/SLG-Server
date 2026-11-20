package loginserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

// ErrCredentials 表示账号不存在、凭据格式无效或密码不匹配。
var ErrCredentials = errors.New("invalid credentials")

// Accounts 在 MySQL 中管理账号及其 bcrypt 密码摘要。
type Accounts struct {
	db *sql.DB
	// 未找到账号时仍执行一次密码比较，减少校验耗时暴露账号存在性的风险。
	dummyHash []byte
}

// NewAccounts 创建账号存储并预先计算用于未知账号校验的摘要。
func NewAccounts(db *sql.DB) (*Accounts, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte("slg unknown account"), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &Accounts{db: db, dummyHash: hash}, nil
}

func validCredentials(account, password string) bool {
	return len(account) > 0 && len(account) <= 64 && utf8.ValidString(account) &&
		len(password) > 0 && len(password) <= 72
}

// Create 创建账号。以相同凭据重试时返回成功，原密码摘要保持不变。
func (a *Accounts) Create(ctx context.Context, account, password string) error {
	if !validCredentials(account, password) {
		return errors.New("account must contain 1–64 UTF-8 bytes and password 1–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx,
		"INSERT INTO slg_accounts (account, password_hash) VALUES (?, ?)", account, hash)
	var duplicate *mysql.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == 1062 {
		if err = a.Authenticate(ctx, account, password); errors.Is(err, ErrCredentials) {
			return errors.New("account already exists with a different password")
		}
	}
	return err
}

// Authenticate 验证账号和密码。凭据错误统一返回 ErrCredentials，数据库故障单独返回。
func (a *Accounts) Authenticate(ctx context.Context, account, password string) error {
	if !validCredentials(account, password) {
		return ErrCredentials
	}
	var hash []byte
	err := a.db.QueryRowContext(ctx, "SELECT password_hash FROM slg_accounts WHERE account = ?", account).Scan(&hash)
	missing := errors.Is(err, sql.ErrNoRows)
	if missing {
		hash = a.dummyHash
	} else if err != nil {
		return fmt.Errorf("look up account: %w", err)
	}
	err = bcrypt.CompareHashAndPassword(hash, []byte(password))
	if missing || errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrCredentials
	}
	return err
}
