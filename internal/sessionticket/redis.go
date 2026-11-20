// Package sessionticket 管理登录服和网关共享的一次性登录票据。
package sessionticket

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const ticketLifetime = 30 * time.Second

// DefaultPrefix 是登录服和网关共用的默认 Redis 键前缀。
const DefaultPrefix = "slg:login"

var errTicket = errors.New("invalid or expired ticket")

// RedisTickets 在 Redis 中保存尚未消费的登录票据及其账号。
type RedisTickets struct {
	client redis.Cmdable
	prefix string
}

// NewRedisTickets 创建票据存储。同一环境的登录服和网关须使用相同的键前缀。
func NewRedisTickets(client redis.Cmdable, prefix string) *RedisTickets {
	return &RedisTickets{client: client, prefix: prefix + ":"}
}

// Issue 签发短期票据，使用 SET NX 保留可能已存在的同名票据。
func (s *RedisTickets) Issue(ctx context.Context, account string) (string, error) {
	ticket := rand.Text()
	ok, err := s.client.SetNX(ctx, s.key(ticket), account, ticketLifetime).Result()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("ticket issuance conflict")
	}
	return ticket, nil
}

// Consume 原子读取并删除票据，同一张票据最多能为一次连接认证获取账号。
func (s *RedisTickets) Consume(ctx context.Context, ticket string) (string, error) {
	if ticket == "" {
		return "", errTicket
	}
	account, err := s.client.GetDel(ctx, s.key(ticket)).Result()
	if errors.Is(err, redis.Nil) {
		return "", errTicket
	}
	return account, err
}

func (s *RedisTickets) key(ticket string) string {
	// 用票据摘要生成键，Redis 中的键名不直接暴露可用于登录的票据。
	sum := sha256.Sum256([]byte(ticket))
	return s.prefix + hex.EncodeToString(sum[:])
}
