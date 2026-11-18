// Package rtsched groups the criticality-based ordering and the OS-level
// (SCHED_FIFO) scheduling helpers, so that both the pod source (package config)
// and the pod workers (package kubelet) can share a single implementation.
package rtsched

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// CriticalityLabel is the pod label carrying the criticality value.
	CriticalityLabel = "criticality"
	// minCriticality and maxCriticality bound the valid criticality range.
	// A value of 0 (or an out-of-range/invalid value) means "no criticality".
	minCriticality = 1
	maxCriticality = 80
)

// schedParam mirrors the C struct sched_param used by sched_setscheduler.
type schedParam struct {
	Sched_priority int32
}

// ReadCriticalityFromSpec returns the pod criticality read from the
// CriticalityLabel. It returns 0 when the label is missing, invalid, or
// outside the range [minCriticality, maxCriticality] (0 means "no criticality").
func ReadCriticalityFromSpec(pod *v1.Pod) int {
	if pod == nil || pod.Labels == nil {
		return 0
	}

	raw := pod.Labels[CriticalityLabel]
	if raw == "" {
		return 0
	}

	criticality, err := strconv.Atoi(raw)
	if err != nil {
		klog.ErrorS(err, "Invalid criticality label, ignoring", "pod", klog.KObj(pod), "criticality", raw)
		return 0
	}
	if criticality < minCriticality || criticality > maxCriticality {
		klog.V(2).InfoS("Criticality out of range, ignoring", "pod", klog.KObj(pod), "criticality", criticality)
		return 0
	}

	klog.V(4).InfoS("Criticality label found", "pod", klog.KObj(pod), "criticality", criticality)
	return criticality
}

// SortPodsByCriticality returns a copy of pods ordered by criticality: lower
// criticality value first (more critical), with pods that have no criticality
// (0) pushed to the end. The sort is stable.
func SortPodsByCriticality(pods []*v1.Pod) []*v1.Pod {
	ordered := append([]*v1.Pod(nil), pods...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := ReadCriticalityFromSpec(ordered[i])
		right := ReadCriticalityFromSpec(ordered[j])

		if left == 0 {
			return false
		}
		if right == 0 {
			return true
		}
		return left < right
	})
	return ordered
}

// RunWithCriticality runs fn under a SCHED_FIFO priority derived from the pod's
// criticality label, on an OS thread locked for the duration of fn, restoring
// the default policy afterwards. If the pod has no valid criticality, fn is run
// as-is, without touching the scheduler.
func RunWithCriticality(pod *v1.Pod, fn func()) {
	criticality := ReadCriticalityFromSpec(pod)
	if criticality <= 0 {
		fn()
		return
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid := unix.Gettid()
	if err := SetThreadScheduler(tid, criticality); err != nil {
		klog.V(2).ErrorS(err, "Failed to set FIFO scheduler", "pod", klog.KObj(pod), "tid", tid, "criticality", criticality)
	}
	defer func() {
		if err := SetThreadScheduler(tid, 0); err != nil {
			klog.V(2).ErrorS(err, "Failed to restore scheduler", "pod", klog.KObj(pod), "tid", tid)
		}
	}()

	fn()
}

// SetThreadScheduler sets the scheduling policy for the thread tid based on the
// given criticality. A criticality of 0 restores the default policy
// (SCHED_OTHER); a value in [minCriticality, maxCriticality] applies SCHED_FIFO.
// Criticality is inverted into a FIFO priority so that a lower criticality
// value (more critical) maps to a higher FIFO priority.
func SetThreadScheduler(tid int, criticality int) error {
	if criticality == 0 {
		return setscheduler(tid, unix.SCHED_NORMAL, 0) // SCHED_NORMAL == POSIX SCHED_OTHER
	}
	if criticality < minCriticality || criticality > maxCriticality {
		return fmt.Errorf("invalid criticality %d, must be %d-%d", criticality, minCriticality, maxCriticality)
	}
	// Lower criticality (more critical) maps to a higher FIFO priority.
	fifoPriority := int32(maxCriticality + 1 - criticality)
	return setscheduler(tid, unix.SCHED_FIFO, fifoPriority)
}

// setscheduler issues the sched_setscheduler syscall for thread tid with the
// given policy and priority.
func setscheduler(tid, policy int, priority int32) error {
	param := schedParam{Sched_priority: priority}
	if _, _, errno := unix.Syscall(
		unix.SYS_SCHED_SETSCHEDULER,
		uintptr(tid),
		uintptr(policy),
		uintptr(unsafe.Pointer(&param)),
	); errno != 0 {
		return fmt.Errorf("sched_setscheduler(tid=%d, policy=%d) failed: %w", tid, policy, errno)
	}
	return nil
}
