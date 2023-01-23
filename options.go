package scheduler

import "github.com/docker/docker/client"

type Option func(scheduler *Scheduler)

func WithDocker(dockerClient *client.Client) Option {
	return func(scheduler *Scheduler) {
		scheduler.client = dockerClient
		scheduler.borrowed = true
	}
}

func WithProject(composeProject string) Option {
	return func(scheduler *Scheduler) {
		scheduler.project = composeProject
	}
}

func WithNotification(notification *HTTPNotification) Option {
	return func(scheduler *Scheduler) {
		scheduler.notification = notification
	}
}
