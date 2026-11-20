package httpserver

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
	"github.com/NumberMan1/orbit"
	"github.com/NumberMan1/orbit/service"
	orbithttp "github.com/NumberMan1/orbit/socket/http"
)

type loggedServer struct {
	*orbithttp.Server
	logger serverlog.Logger
}

func (s loggedServer) Start(ctx context.Context) error {
	if err := s.Server.Start(ctx); err != nil {
		return err
	}
	s.logger.Info("HTTP server listening", field.String("address", s.Addr().String()))
	return nil
}

// Logged 包装 HTTP 组件，在监听成功后记录实际绑定的地址。
func Logged(logger serverlog.Logger, server *orbithttp.Server) orbit.Component {
	return loggedServer{Server: server, logger: logger}
}

// Run 启动所给组件，收到中断或终止信号后触发有时限的优雅停机。
func Run(components ...orbit.Component) error {
	app, err := service.NewApp(5*time.Second, components...)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.RunGraceful(ctx)
}
