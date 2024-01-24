package main

import (
	"github.com/goatapp/ratelimit/src/service_cmd/runner"
	"github.com/goatapp/ratelimit/src/settings"
)

func main() {
	runner := runner.NewRunner(settings.NewSettings())
	runner.Run()
}
