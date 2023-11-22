/*
Copyright 2022 The Katalyst Authors.

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

package numainterpodaffinity

import (
	"context"

	"github.com/kubewharf/katalyst-core/pkg/scheduler/cache"
	schedulerUtil "github.com/kubewharf/katalyst-core/pkg/scheduler/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (na *NUMAInterPodAffinity) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	if schedulerUtil.IsDedicatedPod(pod) && schedulerUtil.IsNumaBinding(pod) {
		podCopy := pod.DeepCopy()
		cache.GetCache().ReservePodAffinity(nodeName, podCopy)
	}
	return framework.NewStatus(framework.Success, "")
}

func (na *NUMAInterPodAffinity) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	if schedulerUtil.IsDedicatedPod(pod) && schedulerUtil.IsNumaBinding(pod) {
		cache.GetCache().UnreservePodAffinity(nodeName, pod)
	}
}
