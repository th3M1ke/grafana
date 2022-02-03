package alerting

import (
	"sync"
	"time" // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - Add import time()

	"github.com/grafana/grafana/pkg/components/null"
)

// Job holds state about when the alert rule should be evaluated.
type Job struct {
	Offset      int64
	OffsetWait  bool
	Delay       bool
	running     bool
	Rule        *Rule
	runningLock sync.Mutex // Lock for running property which is used in the Scheduler and AlertEngine execution
	EvalTime    time.Time // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - add time and result.

	Result *EvalContext // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - add time and result.
}

// GetRunning returns true if the job is running. A lock is taken and released on the Job to ensure atomicity.
func (j *Job) GetRunning() bool {
	defer j.runningLock.Unlock()
	j.runningLock.Lock()
	return j.running
}

// SetRunning sets the running property on the Job. A lock is taken and released on the Job to ensure atomicity.
func (j *Job) SetRunning(b bool) {
	j.runningLock.Lock()
	j.running = b
	j.runningLock.Unlock()
}

// ResultLogEntry represents log data for the alert evaluation.
type ResultLogEntry struct {
	Message string
	Data    interface{}
}

// EvalMatch represents the series violating the threshold.
type EvalMatch struct {
	Value  null.Float        `json:"value"`
	Metric string            `json:"metric"`
	Tags   map[string]string `json:"tags"`
}
