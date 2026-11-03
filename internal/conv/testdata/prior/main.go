// Command prior gives flag destinations initial values instead of defaults.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

type config struct {
	Port    int
	Timeout time.Duration
	Debug   bool
}

func main() {
	region := "us-east-1"
	cfg := config{Port: 8080, Timeout: 5 * time.Second, Debug: true}
	app := kingpin.New("prior", "Initial values.")
	app.Flag("region", "Region.").StringVar(&region)
	app.Flag("port", "Port.").IntVar(&cfg.Port)
	app.Flag("timeout", "Timeout.").DurationVar(&cfg.Timeout)
	app.Flag("debug", "Debug.").BoolVar(&cfg.Debug)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	fmt.Println(region, cfg.Port, cfg.Timeout, cfg.Debug)
}
