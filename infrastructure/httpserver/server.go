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

func Run(logger serverlog.Logger, server *orbithttp.Server, dependencies ...orbit.Component) error {
	components := append(dependencies, loggedServer{Server: server, logger: logger})
	app, err := service.NewApp(5*time.Second, components...)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.RunGraceful(ctx)
}
