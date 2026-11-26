package proxmox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	gp "github.com/luthermonson/go-proxmox"
)

// DefaultTaskPollInterval is how often a task's status is polled while the
// provider waits for it.
const DefaultTaskPollInterval = 2 * time.Second

// nodeFromUPID extracts the node name from a UPID of the form
// "UPID:node:pid:pstart:starttime:type:id:user:". It returns "" if upid is
// not in that form.
func nodeFromUPID(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) < 9 || parts[0] != "UPID" {
		return ""
	}
	return parts[1]
}

// waitTask polls a task at the provider's task poll interval until it stops
// and returns it. A task that stops with a failing exit status yields a
// *gp.TaskFailedError alongside the task. The node is taken from the UPID.
// Cancelling ctx stops the wait, not the task.
func (p *Provider) waitTask(ctx context.Context, upid string) (*gp.Task, error) {
	if nodeFromUPID(upid) == "" {
		return nil, fmt.Errorf("proxmox: malformed UPID %q", upid)
	}
	t := gp.NewTask(gp.UPID(upid), p.client)
	err := t.Wait(ctx, p.taskPoll, waitBudget(ctx))
	if errors.Is(err, gp.ErrTimeout) && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if gp.IsTaskFailed(err) {
			return t, err
		}
		return nil, err
	}
	return t, nil
}

// wait waits for a task; an empty UPID means the server finished
// synchronously.
func (p *Provider) wait(ctx context.Context, upid string) error {
	if upid == "" {
		return nil
	}
	_, err := p.waitTask(ctx, upid)
	return err
}

// waitBudget converts ctx's deadline into the library's mandatory maximum
// wait. Without a deadline the wait is bounded only by cancellation.
func waitBudget(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		// The context fires first; the small margin keeps the library's own
		// timer from winning the race and reporting a timeout instead.
		return time.Until(dl) + time.Second
	}
	return time.Duration(math.MaxInt64)
}
