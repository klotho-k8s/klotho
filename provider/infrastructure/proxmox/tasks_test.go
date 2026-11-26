package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gp "github.com/luthermonson/go-proxmox"
)

const taskStatusPath = "GET /nodes/pve1/tasks/" + testUPID + "/status"

func taskStatusJSON(status, exit string) string {
	exitField := ""
	if exit != "" {
		exitField = fmt.Sprintf(`,"exitstatus":%q`, exit)
	}
	return fmt.Sprintf(`{"data":{"upid":%q,"node":"pve1","pid":703710,"pstart":123456789,"starttime":1705095875,"type":"qmclone","id":"9000","user":"root@pam!klotho","status":%q%s}}`,
		testUPID, status, exitField)
}

func TestWaitTaskRunningThenOK(t *testing.T) {
	var polls atomic.Int32
	p := newRouteServer(t, routes{
		taskStatusPath: func(w http.ResponseWriter, r *http.Request) {
			if polls.Add(1) < 3 {
				rawJSON(taskStatusJSON("running", ""))(w, r)
				return
			}
			rawJSON(taskStatusJSON("stopped", "OK"))(w, r)
		},
	})
	st, err := p.waitTask(context.Background(), testUPID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsSuccessful || st.ExitStatus != "OK" || st.Type != "qmclone" || st.Node != "pve1" || string(st.UPID) != testUPID {
		t.Fatalf("status = %+v", st)
	}
	if n := polls.Load(); n != 3 {
		t.Fatalf("polls = %d, want 3", n)
	}
}

func TestWaitTaskWarnings(t *testing.T) {
	p := newRouteServer(t, routes{taskStatusPath: rawJSON(taskStatusJSON("stopped", "WARNINGS: 2"))})
	st, err := p.waitTask(context.Background(), testUPID)
	if err != nil || !st.IsSuccessful || st.Warnings != 2 {
		t.Fatalf("waitTask = %+v, %v", st, err)
	}
	// wait reports success for warnings too, and nothing to wait for.
	if err := p.wait(context.Background(), testUPID); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTaskFailure(t *testing.T) {
	const exit = "clone failed: storage 'local-lvm' is not online"
	p := newRouteServer(t, routes{taskStatusPath: rawJSON(taskStatusJSON("stopped", exit))})
	st, err := p.waitTask(context.Background(), testUPID)
	var taskErr *gp.TaskFailedError
	if !errors.As(err, &taskErr) {
		t.Fatalf("err = %v, want *gp.TaskFailedError", err)
	}
	if taskErr.ExitStatus != exit || string(taskErr.UPID) != testUPID || taskErr.Node != "pve1" || taskErr.Type != "qmclone" {
		t.Errorf("taskErr = %+v", taskErr)
	}
	if st == nil || st.Status != gp.TaskStopped || st.IsSuccessful {
		t.Errorf("status = %+v", st)
	}
}

func TestWaitTaskContextDeadline(t *testing.T) {
	p := newRouteServer(t, routes{taskStatusPath: rawJSON(taskStatusJSON("running", ""))})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.waitTask(ctx, testUPID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestWaitTaskContextCancel(t *testing.T) {
	p := newRouteServer(t, routes{taskStatusPath: rawJSON(taskStatusJSON("running", ""))})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, err := p.waitTask(ctx, testUPID); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
}

func TestWaitTaskStatusError(t *testing.T) {
	p := newRouteServer(t, routes{taskStatusPath: fail(t, 403, "Permission check failed")})
	_, err := p.waitTask(context.Background(), testUPID)
	apiErr, ok := gp.AsAPIError(err)
	if !ok || apiErr.StatusCode != 403 {
		t.Fatalf("err = %v, want *gp.APIError 403", err)
	}
}

func TestMalformedUPID(t *testing.T) {
	p := newRouteServer(t, routes{})
	for _, upid := range []string{"bogus", "UPID:pve1:1:2:3:x:y"} {
		if _, err := p.waitTask(context.Background(), upid); err == nil {
			t.Errorf("waitTask(%q) accepted malformed UPID", upid)
		}
	}
}

func TestNodeFromUPID(t *testing.T) {
	if got := nodeFromUPID(testUPID); got != "pve1" {
		t.Errorf("nodeFromUPID = %q", got)
	}
	if got := nodeFromUPID("garbage"); got != "" {
		t.Errorf("nodeFromUPID(garbage) = %q", got)
	}
}
