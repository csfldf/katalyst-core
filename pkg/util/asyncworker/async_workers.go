package asyncworker

import (
	"context"
	"fmt"
	"time"

	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

func NewAsyncWorkers(name string) *AsyncWorkers {
	return &AsyncWorkers{
		name:                name,
		lastUndeliveredWork: make(map[string]*Work),
		workStatuses:        make(map[string]*workStatus),
	}
}

func (aws *AsyncWorkers) AddWork(workName string, work *Work) error {
	aws.workLock.Lock()
	defer aws.workLock.Unlock()

	err := validateWork(work)

	if err != nil {
		return fmt.Errorf("validateWork for: %s failed with error: %v", workName, err)
	}

	general.InfoS("add work",
		"AsyncWorkers", aws.name,
		"workName", workName,
		"params", work.Params,
		"deliveredAt", work.DeliveredAt)

	status, ok := aws.workStatuses[workName]

	if !ok || status == nil {
		general.InfoS("create status for work",
			"AsyncWorkers", aws.name, "workName", workName)
		status = &workStatus{}
		aws.workStatuses[workName] = status
	}

	// dispatch a request to the pod work if none are running
	if !status.IsWorking() {
		general.InfoS("status isn't working, handle work immediately",
			"AsyncWorkers", aws.name,
			"workName", workName,
			"params", work.Params,
			"deliveredAt", work.DeliveredAt)

		ctx := aws.contextForWork(workName, work)

		go func() {
			defer runtime.HandleCrash()
			aws.handleWork(ctx, workName, work)
		}()

		return nil
	}

	general.InfoS("status is working, queue work",
		"AsyncWorkers", aws.name,
		"workName", workName,
		"params", work.Params,
		"deliveredAt", work.DeliveredAt)

	if undelivered, ok := aws.lastUndeliveredWork[workName]; ok {
		general.InfoS("overwrite undelivered work",
			"AsyncWorkers", aws.name,
			"workName", workName,
			"old params", undelivered.Params,
			"old deliveredAt", undelivered.DeliveredAt,
			"new params", work.Params,
			"new deliveredAt", work.DeliveredAt)
	}

	// always set the most recent work
	aws.lastUndeliveredWork[workName] = work

	if status.cancelFn == nil {
		general.Fatalf("[AsyncWorkers: %s] %s nil cancelFn in working status", aws.name, workName)
	} else if status.work == nil {
		general.Fatalf("[AsyncWorkers: %s] %s nil work in working status", aws.name, workName)
	}

	general.InfoS("cancelling current working work",
		"AsyncWorkers", aws.name,
		"workName", workName,
		"params", status.work.Params,
		"deliveredAt", status.work.DeliveredAt)
	status.cancelFn()

	return nil
}

func (aws *AsyncWorkers) handleWork(ctx context.Context, workName string, work *Work) error {
	general.InfoS("handle work",
		"AsyncWorkers", aws.name,
		"workName", workName,
		"params", work.Params,
		"deliveredAt", work.DeliveredAt)

	err := work.Fn(ctx, work.Params...)

	aws.completeWork(workName, work, err)

	return nil
}

func (aws *AsyncWorkers) completeWork(workName string, completedWork *Work, workErr error) {
	// TODO: support retrying if workErr != nil
	general.InfoS("complete work",
		"AsyncWorkers", aws.name,
		"workName", workName,
		"params", completedWork.Params,
		"deliveredAt", completedWork.DeliveredAt,
		"workErr", workErr)

	aws.workLock.Lock()
	defer aws.workLock.Unlock()

	if work, exists := aws.lastUndeliveredWork[workName]; exists {

		ctx := aws.contextForWork(workName, work)

		go func() {
			defer runtime.HandleCrash()
			aws.handleWork(ctx, workName, work)
		}()
		delete(aws.lastUndeliveredWork, workName)
	} else {
		aws.resetWorkStatus(workName)
	}
}

// contextForWork returns or initializes the appropriate context for a known
// work. And point status.work to the work. If the current context is expired, it is reset.
// It should be called in function protected by aws.workLock.
func (aws *AsyncWorkers) contextForWork(workName string, work *Work) context.Context {
	if work == nil {
		general.Fatalf("[AsyncWorkers: %s] contextForWork: %s got nil work",
			aws.name, workName)
	}

	status, ok := aws.workStatuses[workName]
	if !ok || status == nil {
		general.Fatalf("[AsyncWorkers: %s] contextForWork: %s got no status",
			aws.name, workName)
	}
	if status.ctx == nil || status.ctx.Err() == context.Canceled {
		status.ctx, status.cancelFn = context.WithCancel(context.Background())
	}
	status.working = true
	status.work = work
	status.startedAt = time.Now()
	return status.ctx
}

// resetWorkStatus resets work status corresponding to workName,
// when there is no work of workName to do.
// It should be called in function protected by aws.workLock.
func (aws *AsyncWorkers) resetWorkStatus(workName string) {
	status, ok := aws.workStatuses[workName]
	if !ok || status == nil {
		general.Fatalf("[AsyncWorkers: %s] contextForWork: %s got no status",
			aws.name, workName)
	}

	status.working = false
	status.work = nil
	status.startedAt = time.Time{}
}

func (aws *AsyncWorkers) Start(stopCh <-chan struct{}) error {
	go wait.Until(aws.cleanupWorkStatus, 10*time.Second, stopCh)
	return nil
}

// cleanupWorkStatus cleans up work status not in working
func (aws *AsyncWorkers) cleanupWorkStatus() {
	aws.workLock.Lock()
	defer aws.workLock.Unlock()

	for workName, status := range aws.workStatuses {
		if status == nil {
			general.Errorf("[AsyncWorkers: %s] nil status for %s, clean it", aws.name, workName)
			delete(aws.workStatuses, workName)
		} else if !status.working {
			general.Errorf("[AsyncWorkers: %s] status for %s not in working, clean it", aws.name, workName)
			delete(aws.workStatuses, workName)
		}
	}
}
