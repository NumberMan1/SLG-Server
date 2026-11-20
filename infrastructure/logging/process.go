// Package logging 管理各服务进程的结构化日志。
package logging

import (
	"fmt"
	"os"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
)

// Run 为服务配置结构化日志，服务退出后关闭日志记录器并返回进程退出码。
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
