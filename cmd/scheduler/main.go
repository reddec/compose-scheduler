package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/jessevdk/go-flags"
	scheduler "github.com/reddec/compose-scheduler"
)

//nolint:gochecknoglobals
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

type Config struct {
	Project string                     `long:"project" env:"PROJECT" description:"Docker compose project, will be automatically detected if not set"`
	Notify  scheduler.HTTPNotification `group:"HTTP notification" namespace:"notify" env-namespace:"NOTIFY"`
}

func main() {
	var config Config
	config.Notify.UserAgent = "scheduler/" + version
	parser := flags.NewParser(&config, flags.Default)
	parser.ShortDescription = "Compose scheduler"
	parser.LongDescription = fmt.Sprintf("Docker compose scheduler\nscheduler %s, commit %s, built at %s by %s\nAuthor: Aleksandr Baryshnikov <owner@reddec.net>", version, commit, date, builtBy)

	if _, err := parser.Parse(); err != nil {
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	var opts []scheduler.Option
	if config.Project != "" {
		opts = append(opts, scheduler.WithProject(config.Project))
	}
	if config.Notify.URL != "" {
		opts = append(opts, scheduler.WithNotification(&config.Notify))
	}
	sc, err := scheduler.Create(ctx, opts...)
	if err != nil {
		log.Panic(err)
	}
	defer sc.Close()
	log.Println("started")
	err = sc.Run(ctx)
	if err != nil {
		log.Panic(err)
	}
	log.Println("finished")
}
