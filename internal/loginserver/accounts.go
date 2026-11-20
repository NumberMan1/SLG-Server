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

var ErrCredentials = errors.New("invalid credentials")

type Accounts struct {
	db *sql.DB
	// An unknown account still incurs one password comparison.
	dummyHash []byte
}

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

// Create adds an account. A retry with the same credentials succeeds without
// changing an existing row. Changing a password requires a separate operation.
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
