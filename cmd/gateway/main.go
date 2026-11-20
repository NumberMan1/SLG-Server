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

	handler := gateway.NewHandler(gateway.DiscoveredServices(directory, servicediscovery.GameServer), client)
	tickets := sessionticket.NewRedisTickets(cache, env.GetEnv("SLG_REDIS_KEY_PREFIX", sessionticket.DefaultPrefix).String())
	sessions := gateway.NewSessions(tickets)
	options, err := gateway.SessionOptions()
	if err != nil {
		return err
	}
	options.Logger = logger.With(field.String("transport", "tcp"))
	tcpServer, err := orbittcp.NewServer(tcpAddr, options, sessions.Connect, nil)
	if err != nil {
		return err
	}
	mux := gateway.WithLogin(handler, gateway.DiscoveredServices(directory, servicediscovery.LoginServer), client, tcpServer)
	webOptions := options
	webOptions.Logger = logger.With(field.String("transport", "websocket"))
	webSessions, err := gateway.NewWebSessions(webOptions, sessions)
	if err != nil {
		return err
	}
	mux = gateway.WithWeb(mux, webSessions, os.Getenv("SLG_WEB_ROOT"))
	server, err := orbithttp.NewServer(addr, mux, nil)
	if err != nil {
		return err
	}
	return httpserver.Run(directory, tcpServer, webSessions, httpserver.Logged(logger, server))
}
