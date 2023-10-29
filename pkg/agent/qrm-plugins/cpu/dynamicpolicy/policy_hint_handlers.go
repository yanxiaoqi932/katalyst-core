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

package dynamicpolicy

import (
	"context"
	"fmt"
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
	pluginapi "k8s.io/kubelet/pkg/apis/resourceplugin/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"

	apiconsts "github.com/kubewharf/katalyst-api/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	cpuutil "github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/util"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/util"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	qosutil "github.com/kubewharf/katalyst-core/pkg/util/qos"
)

// Record all numa level affinity information on numa
type numaInfo struct {
	labels                        map[string][]string
	socketID                      int
	numaID                        int
	AntiAffinityRequiredSelectors []apiconsts.Selector
}

// Record numa level affinity information on pod
type podInfo struct {
	labels                        map[string]string
	AffinityRequiredSelectors     []apiconsts.Selector
	AntiAffinityRequiredSelectors []apiconsts.Selector
}

type preFilterState struct {
	// A map of topology pairs to the number of existing pods that has anti-affinity seletor that match the "pod".
	existingAntiAffinityCounts util.TopologyAffinityCount
	// A map of topology pairs to the number of existing pods that match the affinity seletor of the "pod".
	affinityCounts util.TopologyAffinityCount
	// A map of topology pairs to the number of existing pods that match the anti-affinity seletor of the "pod".
	antiAffinityCounts   util.TopologyAffinityCount
	podAffinityInfo      podInfo
	numaAffinityInfoList []numaInfo
}

func (p *DynamicPolicy) sharedCoresHintHandler(_ context.Context,
	req *pluginapi.ResourceRequest) (*pluginapi.ResourceHintsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("got nil request")
	}

	return util.PackResourceHintsResponse(req, string(v1.ResourceCPU),
		map[string]*pluginapi.ListOfTopologyHints{
			string(v1.ResourceCPU): nil, // indicates that there is no numa preference
		})
}

func (p *DynamicPolicy) reclaimedCoresHintHandler(ctx context.Context,
	req *pluginapi.ResourceRequest) (*pluginapi.ResourceHintsResponse, error) {
	return p.sharedCoresHintHandler(ctx, req)
}

func (p *DynamicPolicy) dedicatedCoresHintHandler(ctx context.Context,
	req *pluginapi.ResourceRequest) (*pluginapi.ResourceHintsResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("dedicatedCoresHintHandler got nil req")
	}

	switch req.Annotations[apiconsts.PodAnnotationMemoryEnhancementNumaBinding] {
	case apiconsts.PodAnnotationMemoryEnhancementNumaBindingEnable:
		return p.dedicatedCoresWithNUMABindingHintHandler(ctx, req)
	default:
		return p.dedicatedCoresWithoutNUMABindingHintHandler(ctx, req)
	}
}

func (p *DynamicPolicy) dedicatedCoresWithNUMABindingHintHandler(_ context.Context,
	req *pluginapi.ResourceRequest) (*pluginapi.ResourceHintsResponse, error) {
	// currently, we set cpuset of sidecar to the cpuset of its main container,
	// so there is no numa preference here.
	if req.ContainerType == pluginapi.ContainerType_SIDECAR {
		return util.PackResourceHintsResponse(req, string(v1.ResourceCPU),
			map[string]*pluginapi.ListOfTopologyHints{
				string(v1.ResourceCPU): nil, // indicates that there is no numa preference
			})
	}

	reqInt, err := util.GetQuantityFromResourceReq(req)
	if err != nil {
		return nil, fmt.Errorf("getReqQuantityFromResourceReq failed with error: %v", err)
	}

	machineState := p.state.GetMachineState()
	var hints map[string]*pluginapi.ListOfTopologyHints

	allocationInfo := p.state.GetAllocationInfo(req.PodUid, req.ContainerName)
	if allocationInfo != nil {
		hints = cpuutil.RegenerateHints(allocationInfo, reqInt)

		// regenerateHints failed. need to clear container record and re-calculate.
		if hints == nil {
			podEntries := p.state.GetPodEntries()
			delete(podEntries[req.PodUid], req.ContainerName)
			if len(podEntries[req.PodUid]) == 0 {
				delete(podEntries, req.PodUid)
			}

			var err error
			machineState, err = generateMachineStateFromPodEntries(p.machineInfo.CPUTopology, podEntries)
			if err != nil {
				general.Errorf("pod: %s/%s, container: %s GenerateMachineStateFromPodEntries failed with error: %v",
					req.PodNamespace, req.PodName, req.ContainerName, err)
				return nil, fmt.Errorf("GenerateMachineStateFromPodEntries failed with error: %v", err)
			}
		}
	}

	// if hints exists in extra state-file, prefer to use them
	if hints == nil {
		availableNUMAs := machineState.GetFilteredNUMASet(state.CheckNUMABinding)

		var extraErr error
		hints, extraErr = util.GetHintsFromExtraStateFile(req.PodName, string(v1.ResourceCPU), p.extraStateFileAbsPath, availableNUMAs)
		if extraErr != nil {
			general.Infof("pod: %s/%s, container: %s GetHintsFromExtraStateFile failed with error: %v",
				req.PodNamespace, req.PodName, req.ContainerName, extraErr)
		}
	}

	// otherwise, calculate hint for container without allocated memory
	if hints == nil {
		var calculateErr error
		// calculate hint for container without allocated cpus
		hints, calculateErr = p.calculateHints(reqInt, machineState, req.Annotations)
		if calculateErr != nil {
			return nil, fmt.Errorf("calculateHints failed with error: %v", calculateErr)
		}
	}

	// start inter-pod affinity & anti-affintiy selection at numa level
	state, err := p.prePodAffinityFilter(req)
	if err != nil {
		hints := make(map[string]*pluginapi.ListOfTopologyHints)
		hints[string(v1.ResourceCPU)] = nil
		resp, _ := util.PackResourceHintsResponse(req, string(v1.ResourceCPU), hints)
		return resp, err
	}
	if state == nil {
		return util.PackResourceHintsResponse(req, string(v1.ResourceCPU), hints)
	}
	filterdHints := p.podAffinityFilter(state, hints)

	return util.PackResourceHintsResponse(req, string(v1.ResourceCPU), filterdHints)
}

func (p *DynamicPolicy) dedicatedCoresWithoutNUMABindingHintHandler(_ context.Context,
	_ *pluginapi.ResourceRequest) (*pluginapi.ResourceHintsResponse, error) {
	// todo: support dedicated_cores without NUMA binding
	return nil, fmt.Errorf("not support dedicated_cores without NUMA binding")
}

// calculateHints is a helper function to calculate the topology hints
// with the given container requests.
func (p *DynamicPolicy) calculateHints(reqInt int, machineState state.NUMANodeMap,
	reqAnnotations map[string]string) (map[string]*pluginapi.ListOfTopologyHints, error) {
	numaNodes := make([]int, 0, len(machineState))
	for numaNode := range machineState {
		numaNodes = append(numaNodes, numaNode)
	}
	sort.Ints(numaNodes)

	hints := map[string]*pluginapi.ListOfTopologyHints{
		string(v1.ResourceCPU): {
			Hints: []*pluginapi.TopologyHint{},
		},
	}

	minNUMAsCountNeeded, _, err := util.GetNUMANodesCountToFitCPUReq(reqInt, p.machineInfo.CPUTopology)
	if err != nil {
		return nil, fmt.Errorf("GetNUMANodesCountToFitCPUReq failed with error: %v", err)
	}

	// because it's hard to control memory allocation accurately,
	// we only support numa_binding but not exclusive container with request smaller than 1 NUMA
	if qosutil.AnnotationsIndicateNUMABinding(reqAnnotations) &&
		!qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) &&
		minNUMAsCountNeeded > 1 {
		return nil, fmt.Errorf("NUMA not exclusive binding container has request larger than 1 NUMA")
	}

	numaPerSocket, err := p.machineInfo.NUMAsPerSocket()
	if err != nil {
		return nil, fmt.Errorf("NUMAsPerSocket failed with error: %v", err)
	}

	bitmask.IterateBitMasks(numaNodes, func(mask bitmask.BitMask) {
		maskCount := mask.Count()
		if maskCount < minNUMAsCountNeeded {
			return
		} else if qosutil.AnnotationsIndicateNUMABinding(reqAnnotations) &&
			!qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) &&
			maskCount > 1 {
			// because it's hard to control memory allocation accurately,
			// we only support numa_binding but not exclusive container with request smaller than 1 NUMA
			return
		}

		maskBits := mask.GetBits()
		numaCountNeeded := mask.Count()

		allAvailableCPUsInMask := machine.NewCPUSet()
		for _, nodeID := range maskBits {
			if machineState[nodeID] == nil {
				general.Warningf("NUMA: %d has nil state", nodeID)
				return
			} else if qosutil.AnnotationsIndicateNUMAExclusive(reqAnnotations) && machineState[nodeID].AllocatedCPUSet.Size() > 0 {
				general.Warningf("numa_exclusive container skip mask: %s with NUMA: %d allocated: %d",
					mask.String(), nodeID, machineState[nodeID].AllocatedCPUSet.Size())
				return
			}

			allAvailableCPUsInMask = allAvailableCPUsInMask.Union(machineState[nodeID].GetAvailableCPUSet(p.reservedCPUs))
		}

		if allAvailableCPUsInMask.Size() < reqInt {
			general.InfofV(4, "available cpuset: %s of size: %d excluding NUMA binding pods which is smaller than request: %d",
				allAvailableCPUsInMask.String(), allAvailableCPUsInMask.Size(), reqInt)
			return
		}

		crossSockets, err := machine.CheckNUMACrossSockets(maskBits, p.machineInfo.CPUTopology)
		if err != nil {
			general.Errorf("CheckNUMACrossSockets failed with error: %v", err)
			return
		} else if numaCountNeeded <= numaPerSocket && crossSockets {
			general.InfofV(4, "needed: %d; min-needed: %d; NUMAs: %v cross sockets with numaPerSocket: %d",
				numaCountNeeded, minNUMAsCountNeeded, maskBits, numaPerSocket)
			return
		}

		hints[string(v1.ResourceCPU)].Hints = append(hints[string(v1.ResourceCPU)].Hints, &pluginapi.TopologyHint{
			Nodes:     machine.MaskToUInt64Array(mask),
			Preferred: len(maskBits) == minNUMAsCountNeeded,
		})
	})

	return hints, nil
}

// Get affinityInfo of all numa nodes
func (p *DynamicPolicy) getNumaNodesAffinityInfo() ([]numaInfo, error) {
	numaResourceMap := p.state.GetMachineState()
	var numaNodesInfo []numaInfo

	for i := 0; i < p.machineInfo.CPUTopology.NumNUMANodes; i++ {
		var numaNodeInfo numaInfo
		numaNodeInfo.numaID = i
		numaNodeInfo.labels = make(map[string][]string)
		cpuSet := p.machineInfo.CPUTopology.CPUDetails.SocketsInNUMANodes(i)
		if cpuSet.Size() == 0 {
			return nil, fmt.Errorf("failed to find the associated socket ID for the specified numanode: %d, cpuDetails: %v", i, p.machineInfo.CPUTopology.CPUDetails)
		}
		numaNodeInfo.socketID = cpuSet.ToSliceInt()[0]

		numaState := numaResourceMap[i]
		for _, containerEntries := range numaState.PodEntries {
			for _, allocationInfo := range containerEntries {
				numaNodeInfo.labels = util.MergeNumaInfoMap(allocationInfo.Labels, numaNodeInfo.labels)
				if allocationInfo.Annotations[apiconsts.PodAnnotationMicroTopologyInterPodAntiAffinity] != "" {
					podAffinity, err := util.UnmarshalAffinity(allocationInfo.Annotations)
					if err != nil {
						return nil, fmt.Errorf("unmarshalAffinity failed")
					}
					if podAffinity.AntiAffinity.Required != nil {
						numaNodeInfo.AntiAffinityRequiredSelectors = append(numaNodeInfo.AntiAffinityRequiredSelectors,
							podAffinity.AntiAffinity.Required...)
					}
				}
				break
			}
		}

		numaNodesInfo = append(numaNodesInfo, numaNodeInfo)
	}

	return numaNodesInfo, nil
}

// Analyze whether the existing pod on NUMA is compatible with the new pod,
// and calculate numa nodes' util.TopologyAffinityCount through imformation of Seletors and labels
func (p *DynamicPolicy) matchNUMAAffinity(Seletors []apiconsts.Selector,
	labels map[string]string, socket int, numa int) util.TopologyAffinityCount {
	topologyMap := make(util.TopologyAffinityCount)
	for _, seletor := range Seletors {
		for key, value := range seletor.MatchLabels {
			if labels[key] == value {
				if seletor.Zone == apiconsts.PodAnnotationMicroTopologyAffinitySocket {
					cpuSet := p.machineInfo.CPUTopology.CPUDetails.NUMANodesInSockets(socket)
					numaList := cpuSet.ToSliceInt()
					for _, n := range numaList {
						topologyMap[n] += 1
					}
				} else {
					topologyMap[numa] += 1
				}
			}
		}
	}
	return topologyMap
}

// Analyze whether the new pod is compatible with the existing pod on NUMA,
// Calculate numa nodes' util.TopologyAffinityCount through imformation of Seletors and labels
func (p *DynamicPolicy) matchPodAffinity(Seletors []apiconsts.Selector,
	labels map[string][]string, socket int, numa int) util.TopologyAffinityCount {
	topologyMap := make(util.TopologyAffinityCount)
	for _, seletor := range Seletors {
		for key, value := range seletor.MatchLabels {
			for _, numaVal := range labels[key] {
				if numaVal == value {
					if seletor.Zone == apiconsts.PodAnnotationMicroTopologyAffinitySocket {
						cpuSet := p.machineInfo.CPUTopology.CPUDetails.NUMANodesInSockets(socket)
						numaList := cpuSet.ToSliceInt()
						for _, n := range numaList {
							topologyMap[n] += 1
						}
					} else {
						topologyMap[numa] += 1
					}
				}
			}

		}
	}
	return topologyMap
}

// Calculate the number of existing pods that has anti-affinity seletor that match the "pod",
// and update the util.TopologyAffinityCount imformation
func (p *DynamicPolicy) getExistingAntiAffinityCounts(state *preFilterState) {
	numNUMA := len(state.numaAffinityInfoList)
	topologyMaps := make([]util.TopologyAffinityCount, numNUMA)

	var wg sync.WaitGroup
	for i := 0; i < numNUMA; i++ {
		wg.Add(1)
		go func(numaID int) {
			defer wg.Done()
			numaAffinity := state.numaAffinityInfoList[numaID]
			topologyMaps[numaID] = p.matchNUMAAffinity(numaAffinity.AntiAffinityRequiredSelectors,
				state.podAffinityInfo.labels, numaAffinity.socketID, numaAffinity.numaID)
		}(i)
	}

	wg.Wait()

	for i := 0; i < numNUMA; i++ {
		state.existingAntiAffinityCounts.Append(topologyMaps[i])
	}

}

// Calculate the number of existing pods that match the anti-affinity seletor of the "pod",
// and update the util.TopologyAffinityCount imformation
func (p *DynamicPolicy) getAntiAffinityCounts(state *preFilterState) {
	numNUMA := len(state.numaAffinityInfoList)
	topologyMaps := make([]util.TopologyAffinityCount, numNUMA)

	var wg sync.WaitGroup
	for i := 0; i < numNUMA; i++ {
		wg.Add(1)
		go func(numaID int) {
			defer wg.Done()
			numaAffinity := state.numaAffinityInfoList[numaID]
			topologyMaps[numaID] = p.matchPodAffinity(state.podAffinityInfo.AntiAffinityRequiredSelectors,
				numaAffinity.labels, numaAffinity.socketID, numaAffinity.numaID)
		}(i)
	}

	wg.Wait()

	for i := 0; i < numNUMA; i++ {
		state.antiAffinityCounts.Append(topologyMaps[i])
	}

}

// Calculate the number of existing pods that match the affinity seletor of the "pod",
// and update the util.TopologyAffinityCount imformation
func (p *DynamicPolicy) getAffinityCounts(state *preFilterState) {
	numNUMA := len(state.numaAffinityInfoList)
	topologyMaps := make([]util.TopologyAffinityCount, numNUMA)

	var wg sync.WaitGroup
	for i := 0; i < numNUMA; i++ {
		wg.Add(1)
		go func(numaID int) {
			defer wg.Done()
			numaAffinity := state.numaAffinityInfoList[numaID]
			topologyMaps[numaID] = p.matchPodAffinity(state.podAffinityInfo.AffinityRequiredSelectors,
				numaAffinity.labels, numaAffinity.socketID, numaAffinity.numaID)
		}(i)
	}

	wg.Wait()

	for i := 0; i < numNUMA; i++ {
		state.affinityCounts.Append(topologyMaps[i])
	}

}

func (p *DynamicPolicy) prePodAffinityFilter(req *pluginapi.ResourceRequest) (*preFilterState, error) {
	podAffinity, err := util.UnmarshalAffinity(req.Annotations)
	if err != nil {
		return nil, err
	}

	// There is no need to do numa level inter-pod affinity selction if exclusive = true
	if req.Annotations[apiconsts.PodAnnotationMemoryEnhancementNumaExclusive] ==
		apiconsts.PodAnnotationMemoryEnhancementNumaExclusiveEnable {
		if podAffinity.Affinity != nil {
			return nil, fmt.Errorf("can not find required affinity numa nodes while exclusive is enabled")
		}
		return nil, nil
	}

	// Get affinityInfo of all numa nodes and the pod to be binded
	numaAffinityInfoList, err := p.getNumaNodesAffinityInfo()
	if err != nil {
		return nil, err
	}
	podAffinityInfo := p.requiredPodAffinityInfo(podAffinity, req)

	// Calculate util.TopologyAffinityCount imformation
	var state = preFilterState{
		podAffinityInfo:            podAffinityInfo,
		numaAffinityInfoList:       numaAffinityInfoList,
		existingAntiAffinityCounts: make(util.TopologyAffinityCount),
		antiAffinityCounts:         make(util.TopologyAffinityCount),
		affinityCounts:             make(util.TopologyAffinityCount),
	}
	p.getExistingAntiAffinityCounts(&state)
	p.getAntiAffinityCounts(&state)
	p.getAffinityCounts(&state)

	return &state, nil
}

// Judge whether hint meets the affinity requirements, true means the hint is valid
func (p *DynamicPolicy) hintPodAffinityFilter(state *preFilterState, hint *pluginapi.TopologyHint) bool {
	numaList := hint.GetNodes()
	for _, numa := range numaList {
		if state.existingAntiAffinityCounts[int(numa)] > 0 {
			return false
		}
		if state.antiAffinityCounts[int(numa)] > 0 {
			return false
		}
		if len(state.podAffinityInfo.AffinityRequiredSelectors) > 0 &&
			state.affinityCounts[int(numa)] <= 0 {
			return false
		}
	}
	return true
}

// Screen all hints through the results of preFilter
func (p *DynamicPolicy) podAffinityFilter(state *preFilterState,
	hints map[string]*pluginapi.ListOfTopologyHints) map[string]*pluginapi.ListOfTopologyHints {
	filterdHints := map[string]*pluginapi.ListOfTopologyHints{
		string(v1.ResourceCPU): {
			Hints: []*pluginapi.TopologyHint{},
		},
	}
	var filterdTopologyHints []*pluginapi.TopologyHint

	for _, hint := range hints[string(v1.ResourceCPU)].Hints {
		if p.hintPodAffinityFilter(state, hint) {
			filterdTopologyHints = append(filterdTopologyHints, hint)
		}
	}
	filterdHints[string(v1.ResourceCPU)].Hints = filterdTopologyHints

	return filterdHints
}

func (p *DynamicPolicy) requiredPodAffinityInfo(podAffinity *util.MicroTopologyPodAffnity, req *pluginapi.ResourceRequest) podInfo {
	var affinityReq []apiconsts.Selector
	var antiAffinityReq []apiconsts.Selector
	if podAffinity.Affinity != nil {
		affinityReq = podAffinity.Affinity.Required
	}
	if podAffinity.AntiAffinity != nil {
		antiAffinityReq = podAffinity.AntiAffinity.Required
	}
	return podInfo{
		labels:                        req.Labels,
		AffinityRequiredSelectors:     affinityReq,
		AntiAffinityRequiredSelectors: antiAffinityReq,
	}
}
