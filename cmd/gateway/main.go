package main

import (
	"context"
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
	"github.com/NumberMan1/numbox/utils/env"
	"github.com/NumberMan1/orbit/discovery"
	orbithttp "github.com/NumberMan1/orbit/socket/http"
	orbittcp "github.com/NumberMan1/orbit/socket/tcp"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/logging"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	"github.com/NumberMan1/slg-server/infrastructure/storage"
	"github.com/NumberMan1/slg-server/internal/gateway"
	"github.com/NumberMan1/slg-server/internal/sessionticket"
)

func main() {
	os.Exit(logging.Run("gateway", run))
}

func run(logger serverlog.Logger) error {
	addr := env.GetEnv("SLG_GATEWAY_ADDR", "127.0.0.1:8080").String()
	tcpAddr := env.GetEnv("SLG_GATEWAY_TCP_ADDR", "127.0.0.1:19090").String()
	cache, err := storage.OpenRedis(context.Background())
	if err != nil {
		return err
	}
	defer cache.Close()
	etcd, registry, err := servicediscovery.Open()
	if err != nil {
		return err
	}
	defer etcd.Close()
	directory, err := registry.NewStore(discovery.StoreOptions{Logger: logger})
	if err != nil {
		return err
	}
	client, err := gateway.NewUpstreamClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	// 单个游戏服拥有试玩世界，网关使用明确配置的内网地址。
	gameURL := env.GetEnv("SLG_GAME_UPSTREAM_URL", "http://127.0.0.1:8081").String()
	if _, err := servicediscovery.ParseOrigin(gameURL); err != nil {
		return err
	}
	gameAddresses := func() []string { return []string{gameURL} }
	handler := gateway.NewHandler(gameAddresses, client)
	world, err := gateway.NewWorldConnector(env.GetEnv("SLG_GAME_RPC_UPSTREAM", "127.0.0.1:8083").String())
	if err != nil {
		return err
	}
	tickets := sessionticket.NewRedisTickets(cache, env.GetEnv("SLG_REDIS_KEY_PREFIX", sessionticket.DefaultPrefix).String())
	sessions := gateway.NewSessions(tickets)
	handleFrame := sessions.WorldHandler(world)
	options, err := gateway.SessionOptions()
	if err != nil {
		return err
	}
	options.Logger = logger.With(field.String("transport", "tcp"))
	tcpServer, err := orbittcp.NewServer(tcpAddr, options, handleFrame, nil)
	if err != nil {
		return err
	}
	loginAddresses := gateway.DiscoveredServices(directory, servicediscovery.LoginServer)
	mux := gateway.WithLogin(handler, loginAddresses, client, tcpServer)
	webOptions := options
	webOptions.Logger = logger.With(field.String("transport", "websocket"))
	webSessions, err := gateway.NewWebSessions(webOptions, handleFrame)
	if err != nil {
		return err
	}
	mux = gateway.WithWeb(mux, webSessions, os.Getenv("SLG_WEB_ROOT"))
	mux = httpserver.WithReadiness(mux, tickets.Ready, func(ctx context.Context) error {
		return gateway.CheckLoginReady(ctx, loginAddresses(), client)
	}, func(ctx context.Context) error {
		return gateway.CheckWorldReady(ctx, world)
	})
	server, err := orbithttp.NewServer(addr, mux, nil)
	if err != nil {
		return err
	}
	// 先启动 TCP 监听器，再允许 HTTP 登录返回它的实际端口。
	return httpserver.Run(directory, world, tcpServer, webSessions, httpserver.Logged(logger, server))
}
