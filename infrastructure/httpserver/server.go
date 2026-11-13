package httpserver

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NumberMan1/orbit/service"
	orbithttp "github.com/NumberMan1/orbit/socket/http"
)

type namedServer struct {
	*orbithttp.Server
	name string
}

func (s namedServer) Start(ctx context.Context) error {
	if err := s.Server.Start(ctx); err != nil {
		return err
	}
	log.Printf("%s listening on %s", s.name, s.Addr())
	return nil
}

func Run(name string, server *orbithttp.Server) error {
	app, err := service.NewApp(5*time.Second, namedServer{Server: server, name: name})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.RunGraceful(ctx)
}
