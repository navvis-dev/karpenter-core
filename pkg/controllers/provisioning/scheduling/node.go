/*
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

package scheduling

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Node is a set of constraints, compatible pods, and possible instance types that could fulfill these constraints. This
// will be turned into one or more actual node instances within the cluster after bin packing.
type Node struct {
	MachineTemplate

	Pods          []*v1.Pod
	topology      *Topology
	hostPortUsage *scheduling.HostPortUsage
}

var nodeID int64

func NewNode(machineTemplate *MachineTemplate, topology *Topology, daemonResources v1.ResourceList, instanceTypes []*cloudprovider.InstanceType) *Node {
	// Copy the template, and add hostname
	hostname := fmt.Sprintf("hostname-placeholder-%04d", atomic.AddInt64(&nodeID, 1))
	topology.Register(v1.LabelHostname, hostname)
	template := *machineTemplate
	template.Requirements = scheduling.NewRequirements()
	template.Requirements.Add(machineTemplate.Requirements.Values()...)
	template.Requirements.Add(scheduling.NewRequirement(v1.LabelHostname, v1.NodeSelectorOpIn, hostname))
	template.InstanceTypeOptions = instanceTypes
	template.Requests = daemonResources

	return &Node{
		MachineTemplate: template,
		hostPortUsage:   scheduling.NewHostPortUsage(),
		topology:        topology,
	}
}

func (m *Node) Add(ctx context.Context, pod *v1.Pod) error {
	// Check Taints
	if err := m.Taints.Tolerates(pod); err != nil {
		return err
	}

	// exposed host ports on the node
	if err := m.hostPortUsage.Validate(pod); err != nil {
		return err
	}

	nodeRequirements := scheduling.NewRequirements(m.Requirements.Values()...)
	podRequirements := scheduling.NewPodRequirements(pod)

	// Check Node Affinity Requirements
	if err := nodeRequirements.Compatible(podRequirements); err != nil {
		return fmt.Errorf("incompatible requirements, %w", err)
	}
	nodeRequirements.Add(podRequirements.Values()...)

	// Check Topology Requirements
	topologyRequirements, err := m.topology.AddRequirements(podRequirements, nodeRequirements, pod)
	if err != nil {
		return err
	}
	if err = nodeRequirements.Compatible(topologyRequirements); err != nil {
		return err
	}
	nodeRequirements.Add(topologyRequirements.Values()...)

	// Check instance type combinations
	requests := resources.Merge(m.Requests, resources.RequestsForPods(pod))
	instanceTypes := filterInstanceTypesByRequirements(m.InstanceTypeOptions, nodeRequirements, requests)
	if len(instanceTypes) == 0 {
		return fmt.Errorf("no instance type satisfied resources %s and requirements %s", resources.String(resources.RequestsForPods(pod)), nodeRequirements)
	}

	// Update node
	m.Pods = append(m.Pods, pod)
	m.InstanceTypeOptions = instanceTypes
	m.Requests = requests
	m.Requirements = nodeRequirements
	m.topology.Record(pod, nodeRequirements)
	m.hostPortUsage.Add(ctx, pod)
	return nil
}

// FinalizeScheduling is called once all scheduling has completed and allows the node to perform any cleanup
// necessary before its requirements are used for instance launching
func (m *Node) FinalizeScheduling() {
	// We need nodes to have hostnames for topology purposes, but we don't want to pass that node name on to consumers
	// of the node as it will be displayed in error messages
	delete(m.Requirements, v1.LabelHostname)
}

func (m *Node) String() string {
	return fmt.Sprintf("node with %d pods requesting %s from types %s", len(m.Pods), resources.String(m.Requests),
		InstanceTypeList(m.InstanceTypeOptions))
}

func InstanceTypeList(instanceTypeOptions []*cloudprovider.InstanceType) string {
	var itSb strings.Builder
	for i, it := range instanceTypeOptions {
		// print the first 5 instance types only (indices 0-4)
		if i > 4 {
			fmt.Fprintf(&itSb, " and %d other(s)", len(instanceTypeOptions)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&itSb, ", ")
		}
		fmt.Fprint(&itSb, it.Name)
	}
	return itSb.String()
}

func filterInstanceTypesByRequirements(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements, requests v1.ResourceList) []*cloudprovider.InstanceType {
	return lo.Filter(instanceTypes, func(instanceType *cloudprovider.InstanceType, _ int) bool {
		return compatible(instanceType, requirements) && fits(instanceType, requests) && hasOffering(instanceType, requirements)
	})
}

func compatible(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) bool {
	return instanceType.Requirements.Intersects(requirements) == nil
}

func fits(instanceType *cloudprovider.InstanceType, requests v1.ResourceList) bool {
	return resources.Fits(resources.Merge(requests, instanceType.Overhead.Total()), instanceType.Capacity)
}

func hasOffering(instanceType *cloudprovider.InstanceType, requirements scheduling.Requirements) bool {
	for _, offering := range instanceType.Offerings.Available() {
		if (!requirements.Has(v1.LabelTopologyZone) || requirements.Get(v1.LabelTopologyZone).Has(offering.Zone)) &&
			(!requirements.Has(v1alpha5.LabelCapacityType) || requirements.Get(v1alpha5.LabelCapacityType).Has(offering.CapacityType)) {
			return true
		}
	}
	return false
}
