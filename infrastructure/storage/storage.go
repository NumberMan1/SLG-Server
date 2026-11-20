// Package storage 根据运行环境配置并验证 MySQL 和 Redis 连接。
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/NumberMan1/numbox/utils/env"
	orbitmysql "github.com/NumberMan1/orbit/storage/mysql"
	orbitredis "github.com/NumberMan1/orbit/storage/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// OpenMySQL 打开 MySQL 连接池并验证连通性，连接池由调用方关闭。
func OpenMySQL(ctx context.Context) (*sql.DB, error) {
	dsn := env.GetEnv("SLG_MYSQL_DSN").String()
	if dsn == "" {
		return nil, errors.New("SLG_MYSQL_DSN is required")
	}
	if address := env.GetEnv("SLG_MYSQL_ADDRESS").String(); address != "" {
		// 宿主机调试仅覆盖连接地址，继续使用容器联调配置中的数据库和凭据。
		config, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, fmt.Errorf("parse MySQL DSN: %w", err)
		}
		config.Net, config.Addr = "tcp", address
		dsn = config.FormatDSN()
	}
	db, err := orbitmysql.Open(ctx, orbitmysql.Options{DSN: dsn, MaxOpen: 16, MaxIdle: 4, MaxLifetime: 5 * time.Minute})
	if err != nil {
		return nil, fmt.Errorf("open MySQL: %w", err)
	}
	return db, nil
}

// OpenRedis 按配置连接单机或集群并验证连通性，客户端由调用方关闭。
func OpenRedis(ctx context.Context) (redis.UniversalClient, error) {
	addresses := strings.Split(env.GetEnv("SLG_REDIS_ADDRESSES", "127.0.0.1:6379").String(), ",")
	for i := range addresses {
		addresses[i] = strings.TrimSpace(addresses[i])
	}
	options := orbitredis.Options{
		Addresses: addresses,
		Username:  env.GetEnv("SLG_REDIS_USERNAME").String(),
		Password:  env.GetEnv("SLG_REDIS_PASSWORD").String(),
	}
	switch env.GetEnv("SLG_REDIS_MODE", "cluster").String() {
	case "cluster":
		options.Cluster = true
		options.ClusterRedirects = 3
	case "standalone":
	default:
		return nil, errors.New("SLG_REDIS_MODE must be cluster or standalone")
	}
	var client redis.UniversalClient
	var err error
	if mappings := env.GetEnv("SLG_REDIS_DIAL_ADDRESSES").String(); mappings != "" {
		if !options.Cluster {
			return nil, errors.New("SLG_REDIS_DIAL_ADDRESSES requires cluster mode")
		}
		client, err = openRedisWithDialAddresses(ctx, options, mappings)
	} else {
		client, err = orbitredis.Open(ctx, options)
	}
	if err != nil {
		return nil, fmt.Errorf("open Redis: %w", err)
	}
	return client, nil
}

func openRedisWithDialAddresses(ctx context.Context, options orbitredis.Options, mappings string) (redis.UniversalClient, error) {
	// 集群发现和重定向仍使用节点公布的地址，实际连接才映射到宿主机端口。
	dialAddresses := make(map[string]string)
	for _, mapping := range strings.Split(mappings, ",") {
		node, target, ok := strings.Cut(strings.TrimSpace(mapping), "=")
		node, target = strings.TrimSpace(node), strings.TrimSpace(target)
		if !ok || node == "" || target == "" || dialAddresses[node] != "" {
			return nil, errors.New("SLG_REDIS_DIAL_ADDRESSES must contain unique node=target pairs")
		}
		dialAddresses[node] = target
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: options.Addresses, Username: options.Username, Password: options.Password,
		Protocol: 2, ContextTimeoutEnabled: true, MaxRetries: -1, MaxRedirects: options.ClusterRedirects,
		ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second,
		// 禁用维护通知的地址探测，避免宿主机 DNS 查询仅在容器网络可用的节点名。
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
		Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
			target, ok := dialAddresses[address]
			if !ok {
				return nil, fmt.Errorf("missing SLG_REDIS_DIAL_ADDRESSES entry for %s", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}
