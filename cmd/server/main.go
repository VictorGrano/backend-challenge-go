package main

import (
	"time"

	"go.uber.org/fx"

	"github.com/VictorGrano/backend-challenge-go/internal/app"
	"github.com/VictorGrano/backend-challenge-go/internal/config"
)

func main() {
	stopTimeout := 20 * time.Second
	if cfg, err := config.Load(); err == nil {
		stopTimeout = cfg.ShutdownTimeout
	}

	app.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(stopTimeout),
	).Run()
}
