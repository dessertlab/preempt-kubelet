/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/rtsched"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// WaitForAPIServerSyncPeriod is the period between checks for the node list/watch initial sync
const WaitForAPIServerSyncPeriod = 1 * time.Second
const (
	enablePodOrdering   = true
	enablePodGating     = true
	targetPodPriorities = 8
	targetPodNamespace  = "test-priority"
	targetPodReplicas   = 5
)

// NewSourceApiserver creates a config source that watches and pulls from the apiserver.
func NewSourceApiserver(logger klog.Logger, c clientset.Interface, nodeName types.NodeName, nodeHasSynced func() bool, updates chan<- interface{}) {
	lw := cache.NewListWatchFromClient(c.CoreV1().RESTClient(), "pods", metav1.NamespaceAll, fields.OneTermEqualSelector("spec.nodeName", string(nodeName)))

	// The Reflector responsible for watching pods at the apiserver should be run only after
	// the node sync with the apiserver has completed.
	logger.Info("Waiting for node sync before watching apiserver pods")
	go func() {
		for {
			if nodeHasSynced() {
				logger.V(4).Info("node sync completed")
				break
			}
			time.Sleep(WaitForAPIServerSyncPeriod)
			logger.V(4).Info("node sync has not completed yet")
		}
		logger.Info("Watching apiserver")
		if enablePodOrdering || enablePodGating {
			newSourceApiserverFromLWWithOrdering(lw, updates)
			return
		}
		newSourceApiserverFromLW(lw, updates)
	}()
}

// newSourceApiserverFromLW holds creates a config source that watches and pulls from the apiserver.
func newSourceApiserverFromLW(lw cache.ListerWatcher, updates chan<- interface{}) {
	send := func(objs []interface{}) {
		var pods []*v1.Pod
		for _, o := range objs {
			pods = append(pods, o.(*v1.Pod))
		}
		updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.ApiserverSource}
	}
	r := cache.NewReflector(lw, &v1.Pod{}, cache.NewUndeltaStore(send, cache.MetaNamespaceKeyFunc), 0)
	go r.Run(wait.NeverStop)
}

func buildExpectedPodNames(priorityLevels, replicas int) map[string]struct{} {
	expected := make(map[string]struct{}, priorityLevels*replicas)
	for priority := 1; priority <= priorityLevels; priority++ {
		for replica := 1; replica <= replicas; replica++ {
			expected[fmt.Sprintf("pod-prio%d-%d", priority, replica)] = struct{}{}
		}
	}
	return expected
}

// Comportamento custom: gating + ordinamento high prima di normal.
func newSourceApiserverFromLWWithOrdering(lw cache.ListerWatcher, updates chan<- interface{}) {
	targetNamespace := targetPodNamespace
	targetPriorities := targetPodPriorities
	targetReplicas := targetPodReplicas
	expectedNames := buildExpectedPodNames(targetPriorities, targetReplicas)
	gateEnabled := targetNamespace != "" && targetReplicas > 0
	gateReleased := !gateEnabled
	awaitingReset := false

	var seenPods sync.Map

	send := func(objs []interface{}) {
		targetPresent := make(map[string]struct{})
		var targetPods []*v1.Pod
		var otherPods []*v1.Pod

		// First pass: separate target-namespace pods from others and record presence.
		// Also log "Parsed pod from apiserver" only the first time a pod UID is seen.
		for _, o := range objs {
			pod := o.(*v1.Pod)

			// Global seen tracking: ensure we only log the first time we encounter a pod UID.
			if _, loaded := seenPods.LoadOrStore(string(pod.UID), struct{}{}); !loaded {
				klog.V(4).Info("Parsed pod from apiserver", "pod", klog.KObj(pod))
			}

			if gateEnabled && !gateReleased && pod.Namespace == targetNamespace {
				targetPresent[pod.Name] = struct{}{}
				targetPods = append(targetPods, pod)
				continue
			}
			otherPods = append(otherPods, pod)
		}

		// Stato Gate: possibly reset if awaitingReset and no target pods are present
		if gateEnabled && awaitingReset && len(targetPresent) == 0 {
			gateReleased = false
			awaitingReset = false
			klog.V(4).Info("Reset apiserver gate state", "namespace", targetNamespace)
		}

		// If gate is enabled and currently closed, check if all expected pods arrived
		if gateEnabled && !gateReleased {
			allArrived := true
			for name := range expectedNames {
				if _, ok := targetPresent[name]; !ok {
					allArrived = false
					break
				}
			}

			if allArrived {
				gateReleased = true
				awaitingReset = true
				// Reset seen maps so pods are considered new after the gate is released
				klog.V(4).Info("Releasing apiserver pod update", "namespace", targetNamespace, "replicas", targetReplicas, "reason", "all desired pods arrived; reset seen maps")
			}
		}

		// Build filtered list: always include non-target pods; include target pods only if gate is released
		filtered := make([]*v1.Pod, 0, len(objs))
		filtered = append(filtered, otherPods...)
		if !gateEnabled || gateReleased {
			filtered = append(filtered, targetPods...)
		}
		var pods []*v1.Pod
		if enablePodOrdering {
			pods = rtsched.SortPodsByCriticality(filtered)
			klog.V(4).Info("Applying custom pod ordering")
		} else {
			pods = filtered
		}

		updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.ApiserverSource}
	}

	r := cache.NewReflector(lw, &v1.Pod{}, cache.NewUndeltaStore(send, cache.MetaNamespaceKeyFunc), 0)
	go r.Run(wait.NeverStop)
}
