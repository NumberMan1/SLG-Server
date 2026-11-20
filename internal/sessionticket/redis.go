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
const DefaultPrefix = "slg:login"

var errTicket = errors.New("invalid or expired ticket")

type RedisTickets struct {
	client redis.Cmdable
	prefix string
}

func NewRedisTickets(client redis.Cmdable, prefix string) *RedisTickets {
	return &RedisTickets{client: client, prefix: prefix + ":"}
}

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
	sum := sha256.Sum256([]byte(ticket))
	return s.prefix + hex.EncodeToString(sum[:])
}
