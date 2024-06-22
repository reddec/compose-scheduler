package scheduler

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/kballard/go-shellquote"
	"github.com/robfig/cron/v3"
)

const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
	schedulerLabel      = "net.reddec.scheduler.cron"
	commandLabel        = "net.reddec.scheduler.exec"
	logsLabel           = "net.reddec.scheduler.logs"
)

func Create(ctx context.Context, options ...Option) (*Scheduler, error) {
	sc := &Scheduler{}
	for _, opt := range options {
		opt(sc)
	}

	if sc.client == nil {
		dockerClient, err := client.NewClientWithOpts(client.FromEnv)
		if err != nil {
			return nil, fmt.Errorf("create docker client: %w", err)
		}
		sc.client = dockerClient
		sc.borrowed = false
	}

	if sc.project == "" {
		project, err := getComposeProject(ctx, sc.client)
		if err != nil {
			_ = sc.Close()
			return nil, fmt.Errorf("get compose project: %w", err)
		}
		sc.project = project
	}
	return sc, nil
}

type Task struct {
	Service   string
	Container string
	Schedule  string
	Command   []string
	logging   bool
}

type Scheduler struct {
	project      string
	client       *client.Client
	borrowed     bool
	notification *HTTPNotification
}

func (sc *Scheduler) Close() error {
	if sc.borrowed {
		return nil
	}
	return sc.client.Close()
}
func (sc *Scheduler) Run(ctx context.Context) error {
	tasks, err := sc.listTasks(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	engine := cron.New()

	for _, t := range tasks {
		log.Println("task for service", t.Service, "at", t.Schedule, "| logging:", t.logging)
		running := new(int32)
		t := t
		_, err = engine.AddFunc(t.Schedule, func() {
			sc.runJob(ctx, running, t)
		})
		if err != nil {
			return fmt.Errorf("add service %s: %w", t.Service, err)
		}
	}

	engine.Start()
	<-ctx.Done()
	<-engine.Stop().Done()

	return nil
}

func (sc *Scheduler) runJob(ctx context.Context, running *int32, t Task) {
	started := time.Now()
	err := sc.runTask(ctx, running, t)
	end := time.Now()
	var errMessage string
	if err != nil {
		errMessage = err.Error()
		log.Println("service", t.Service, "failed after", end.Sub(started), "with error:", err)
	} else {
		log.Println("service", t.Service, "finished after", end.Sub(started), "successfully")
	}
	if sc.notification == nil {
		return
	}
	err = sc.notification.Notify(ctx, &Payload{
		Project:   sc.project,
		Service:   t.Service,
		Container: t.Container,
		Schedule:  t.Schedule,
		Started:   started,
		Finished:  end,
		Failed:    err != nil,
		Error:     errMessage,
	})
	if err != nil {
		log.Println("notification for service", t.Service, "failed:", err)
	} else {
		log.Println("notification for service", t.Service, "succeeded")
	}
}

func (sc *Scheduler) runTask(ctx context.Context, running *int32, task Task) error {
	if !atomic.CompareAndSwapInt32(running, 0, 1) {
		return fmt.Errorf("task is running")
	}
	defer atomic.StoreInt32(running, 0)

	if len(task.Command) == 0 {
		log.Println("running service", task.Service)
		return sc.runService(ctx, task)
	}
	log.Println("executing service", task.Service, "with command", task.Command)
	return sc.execService(ctx, task)
}

func (sc *Scheduler) execService(ctx context.Context, task Task) error {
	if task.logging {
		return sc.execAttachService(ctx, task)
	} else {
		return sc.execStartService(ctx, task)
	}
}

func (sc *Scheduler) execStartService(ctx context.Context, task Task) error {
	execID, err := sc.client.ContainerExecCreate(ctx, task.Container, types.ExecConfig{
		Cmd: task.Command,
	})
	if err != nil {
		return fmt.Errorf("create exec for %s: %w", task.Service, err)
	}

	err = sc.client.ContainerExecStart(ctx, execID.ID, types.ExecStartCheck{})
	if err != nil {
		return fmt.Errorf("exec for %s: %w", task.Service, err)
	}
	return nil
}

func (sc *Scheduler) execAttachService(ctx context.Context, task Task) error {
	execID, err := sc.client.ContainerExecCreate(ctx, task.Container, types.ExecConfig{
		Cmd:          task.Command,
		AttachStderr: true,
		AttachStdout: true,
	})
	if err != nil {
		return fmt.Errorf("create exec for %s: %w", task.Service, err)
	}

	attach, err := sc.client.ContainerExecAttach(ctx, execID.ID, types.ExecStartCheck{})
	if err != nil {
		return fmt.Errorf("exec for %s: %w", task.Service, err)
	}
	defer attach.Close()
	io.Copy(log.Writer(), attach.Reader)

	inspect, err := sc.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("inspect exec for %s: %w", task.Service, err)
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("command returned non-zero code %d", inspect.ExitCode)
	}
	return nil
}

func (sc *Scheduler) runService(ctx context.Context, task Task) error {
	err := sc.client.ContainerStart(ctx, task.Container, types.ContainerStartOptions{})
	if err != nil {
		return fmt.Errorf("start service %s: %w", task.Service, err)
	}
	ok, failed := sc.client.ContainerWait(ctx, task.Container, container.WaitConditionNotRunning)
	select {
	case res := <-ok:
		if res.Error != nil {
			return fmt.Errorf("service %s: %s", task.Service, res.Error.Message)
		}
		if res.StatusCode != 0 {
			return fmt.Errorf("service %s: status code %d", task.Service, res.StatusCode)
		}
	case err = <-failed:
		return fmt.Errorf("wait for service %s: %w", task.Service, err)
	}
	return nil
}

func (sc *Scheduler) listTasks(ctx context.Context) ([]Task, error) {
	list, err := sc.client.ContainerList(ctx, types.ContainerListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", composeProjectLabel+"="+sc.project),
			filters.Arg("label", composeServiceLabel),
			filters.Arg("label", schedulerLabel),
		),
		All: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list container: %w", err)
	}
	var ans = make([]Task, 0, len(list))
	for _, c := range list {
		service := c.Labels[composeServiceLabel]
		var args []string
		if v := c.Labels[commandLabel]; v != "" {
			cmd, err := shellquote.Split(v)
			if err != nil {
				return nil, fmt.Errorf("parse command in service %s: %w", service, err)
			}
			args = cmd
		}

		isLoggingEnabled, err := strconv.ParseBool(c.Labels[logsLabel])
		if err != nil {
			isLoggingEnabled = false
		}

		ans = append(ans, Task{
			Container: c.ID,
			Schedule:  c.Labels[schedulerLabel],
			Service:   service,
			Command:   args,
			logging:   isLoggingEnabled,
		})
	}

	return ans, nil
}

func containerID() (string, error) {
	const path = `/proc/1/cpuset`
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("detect container ID: %w", err)
	}
	id := filepath.Base(strings.TrimSpace(string(data)))
	if id == "/" || id == ".." {
		return "", fmt.Errorf("calculate container ID from %s: %w", string(data), err)
	}

	return id, nil
}

func getComposeProject(ctx context.Context, dockerClient *client.Client) (string, error) {
	var cID string
	for _, lookup := range containerIDLookup {
		v, err := lookup()
		if err == nil {
			cID = v
			break
		}
	}

	if cID == "" {
		return "", fmt.Errorf("failed detect self container ID")
	}

	info, err := dockerClient.ContainerInspect(ctx, cID)
	if err != nil {
		return "", fmt.Errorf("inspect self container: %w", err)
	}
	project, ok := info.Config.Labels[composeProjectLabel]
	if !ok {
		return "", fmt.Errorf("compose label not found - probably container is not part of compose")
	}
	return project, nil
}

func containerIDv2() (string, error) {
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	idRegex := regexp.MustCompile(`containers/([[:alnum:]]{64})/`)
	matches := idRegex.FindStringSubmatch(string(content))
	if len(matches) == 0 {
		return "", fmt.Errorf("no container id")
	}
	return matches[len(matches)-1], nil
}

var containerIDLookup = []func() (string, error){
	containerID, containerIDv2,
}
