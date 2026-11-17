package main

import (
	"flag"
	"fmt"
	"os"

	"photography-workflow/server/internal/config"
	"photography-workflow/server/internal/db"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "path to config file")
	direction := flag.String("dir", "up", "up|down|status|version")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(cfg.Database.MigrateURL, *direction); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}
