package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/numbox/utils/env"
	orbithttp "github.com/NumberMan1/orbit/socket/http"

	"github.com/NumberMan1/slg-server/database"
	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/logging"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	"github.com/NumberMan1/slg-server/infrastructure/storage"
	"github.com/NumberMan1/slg-server/internal/loginserver"
	"github.com/NumberMan1/slg-server/internal/sessionticket"
)

func main() {
	os.Exit(logging.Run(servicediscovery.LoginServer, run))
}

func run(logger serverlog.Logger) error {
	// 初始化数据库或创建账号时执行一次性命令，常驻模式负责接收登录请求。
	if len(os.Args) > 1 {
		return runCommand(os.Args[1:])
	}
	limits, err := loadAdmissionLimits()
	if err != nil {
		return err
	}
	addr := env.GetEnv("SLG_LOGIN_ADDR", "127.0.0.1:8082").String()
	advertise := env.GetEnv("SLG_LOGIN_ADVERTISE_URL", env.EnvValue("http://"+addr)).String()
	ctx := context.Background()
	db, err := storage.OpenMySQL(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	accounts, err := loginserver.NewAccounts(db)
	if err != nil {
		return err
	}
	cache, err := storage.OpenRedis(ctx)
	if err != nil {
		return err
	}
	defer cache.Close()
	etcd, registry, err := servicediscovery.Open()
	if err != nil {
		return err
	}
	defer etcd.Close()
	registrar, err := servicediscovery.NewHTTPRegistrar(registry, servicediscovery.LoginServer, os.Getenv("SLG_LOGIN_INSTANCE_ID"), advertise)
	if err != nil {
		return fmt.Errorf("register login service: %w", err)
	}
	tickets := sessionticket.NewRedisTickets(cache, env.GetEnv("SLG_REDIS_KEY_PREFIX", sessionticket.DefaultPrefix).String())
	handler := httpserver.WithReadiness(loginserver.NewHandler(accounts, tickets, logger, limits), db.PingContext, tickets.Ready)
	server, err := orbithttp.NewServer(addr, handler, nil)
	if err != nil {
		return err
	}
	// HTTP 监听成功后才将登录服公布给网关。
	return httpserver.Run(httpserver.Logged(logger, server), registrar)
}

func runCommand(args []string) error {
	if len(args) != 1 || (args[0] != "migrate" && args[0] != "create-account") {
		return errors.New("usage: login-server [migrate|create-account]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := storage.OpenMySQL(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if args[0] == "migrate" {
		return database.Migrate(ctx, db)
	}
	accounts, err := loginserver.NewAccounts(db)
	if err != nil {
		return err
	}
	return accounts.Create(ctx, os.Getenv("SLG_ACCOUNT"), os.Getenv("SLG_ACCOUNT_PASSWORD"))
}
