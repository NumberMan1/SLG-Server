package logging

import (
	"fmt"
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
)

// Run owns the logger until the service and its components have stopped.
func Run(service string, run func(serverlog.Logger) error) int {
	core, err := serverlog.NewConfiguredLogger(serverlog.Config{
		Stdout:    true,
		StdoutTyp: "json",
		Fields:    serverlog.M{"service": service},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "configure logger:", err)
		return 1
	}
	defer core.Close()

	logger := serverlog.NewServerLogger(core)
	if err := run(logger); err != nil {
		logger.Error("service failed", field.Error(err))
		return 1
	}
	return 0
}
