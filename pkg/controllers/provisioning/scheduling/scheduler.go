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
	"sort"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// SchedulerOptions can be used to control the scheduling, these options are currently only used during consolidation.
type SchedulerOptions struct {
	// SimulationMode if true will prevent recording of the pod nomination decisions as events
	SimulationMode bool
}

func NewScheduler(ctx context.Context, kubeClient client.Client, machines []*MachineTemplate,
	provisioners []v1alpha5.Provisioner, cluster *state.Cluster, stateNodes []*state.Node, topology *Topology,
	instanceTypes map[string][]*cloudprovider.InstanceType, daemonOverhead map[*MachineTemplate]v1.ResourceList,
	recorder events.Recorder, opts SchedulerOptions) *Scheduler {

	// if any of the provisioners add a taint with a prefer no schedule effect, we add a toleration for the taint
	// during preference relaxation
	toleratePreferNoSchedule := false
	for _, prov := range provisioners {
		for _, taint := range prov.Spec.Taints {
			if taint.Effect == v1.TaintEffectPreferNoSchedule {
				toleratePreferNoSchedule = true
			}
		}
	}

	s := &Scheduler{
		ctx:                ctx,
		kubeClient:         kubeClient,
		machineTemplates:   machines,
		topology:           topology,
		cluster:            cluster,
		instanceTypes:      instanceTypes,
		daemonOverhead:     daemonOverhead,
		recorder:           recorder,
		opts:               opts,
		preferences:        &Preferences{ToleratePreferNoSchedule: toleratePreferNoSchedule},
		remainingResources: map[string]v1.ResourceList{},
	}

	namedNodeTemplates := lo.KeyBy(s.machineTemplates, func(nodeTemplate *MachineTemplate) string {
		return nodeTemplate.Requirements.Get(v1alpha5.ProvisionerNameLabelKey).Values()[0]
	})

	for _, provisioner := range provisioners {
		if provisioner.Spec.Limits != nil {
			s.remainingResources[provisioner.Name] = provisioner.Spec.Limits.Resources
		}
	}

	s.calculateExistingMachines(namedNodeTemplates, stateNodes)
	return s
}

type Scheduler struct {
	ctx                context.Context
	newNodes           []*Node
	existingNodes      []*ExistingNode
	machineTemplates   []*MachineTemplate
	remainingResources map[string]v1.ResourceList // provisioner name -> remaining resources for that provisioner
	instanceTypes      map[string][]*cloudprovider.InstanceType
	daemonOverhead     map[*MachineTemplate]v1.ResourceList
	preferences        *Preferences
	topology           *Topology
	cluster            *state.Cluster
	recorder           events.Recorder
	opts               SchedulerOptions
	kubeClient         client.Client
}

func (s *Scheduler) Solve(ctx context.Context, pods []*v1.Pod) ([]*Node, []*ExistingNode, error) {
	// We loop trying to schedule unschedulable pods as long as we are making progress.  This solves a few
	// issues including pods with affinity to another pod in the batch. We could topo-sort to solve this, but it wouldn't
	// solve the problem of scheduling pods where a particular order is needed to prevent a max-skew violation. E.g. if we
	// had 5xA pods and 5xB pods were they have a zonal topology spread, but A can only go in one zone and B in another.
	// We need to schedule them alternating, A, B, A, B, .... and this solution also solves that as well.
	errors := map[*v1.Pod]error{}
	q := NewQueue(pods...)
	for {
		// Try the next pod
		pod, ok := q.Pop()
		if !ok {
			break
		}

		// Schedule to existing nodes or create a new node
		if errors[pod] = s.add(ctx, pod); errors[pod] == nil {
			continue
		}

		// If unsuccessful, relax the pod and recompute topology
		relaxed := s.preferences.Relax(ctx, pod)
		q.Push(pod, relaxed)
		if relaxed {
			if err := s.topology.Update(ctx, pod); err != nil {
				logging.FromContext(ctx).Errorf("updating topology, %s", err)
			}
		}
	}

	for _, n := range s.newNodes {
		n.FinalizeScheduling()
	}
	if !s.opts.SimulationMode {
		s.recordSchedulingResults(ctx, pods, q.List(), errors)
	}
	return s.newNodes, s.existingNodes, nil
}

func (s *Scheduler) recordSchedulingResults(ctx context.Context, pods []*v1.Pod, failedToSchedule []*v1.Pod, errors map[*v1.Pod]error) {
	// Report failures and nominations
	for _, pod := range failedToSchedule {
		logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(pod)).Errorf("Could not schedule pod, %s", errors[pod])
		s.recorder.Publish(events.PodFailedToSchedule(pod, errors[pod]))
	}

	for _, node := range s.existingNodes {
		if len(node.Pods) > 0 {
			s.cluster.NominateNodeForPod(node.Node.Name)
		}
		for _, pod := range node.Pods {
			s.recorder.Publish(events.NominatePod(pod, node.Node))
		}
	}

	// Report new nodes, or exit to avoid log spam
	newCount := 0
	for _, node := range s.newNodes {
		newCount += len(node.Pods)
	}
	if newCount == 0 {
		return
	}
	logging.FromContext(ctx).With("pods", len(pods)).Infof("found provisionable pod(s)")
	logging.FromContext(ctx).With("newNodes", len(s.newNodes), "pods", newCount).Infof("computed new node(s) to fit pod(s)")
	// Report in flight newNodes, or exit to avoid log spam
	inflightCount := 0
	existingCount := 0
	for _, node := range lo.Filter(s.existingNodes, func(node *ExistingNode, _ int) bool { return len(node.Pods) > 0 }) {
		inflightCount++
		existingCount += len(node.Pods)
	}
	if existingCount == 0 {
		return
	}
	logging.FromContext(ctx).Infof("computed %d unready node(s) will fit %d pod(s)", inflightCount, existingCount)
}

func (s *Scheduler) add(ctx context.Context, pod *v1.Pod) error {
	// first try to schedule against an in-flight real node
	for _, node := range s.existingNodes {
		if err := node.Add(ctx, pod); err == nil {
			return nil
		}
	}

	// Consider using https://pkg.go.dev/container/heap
	sort.Slice(s.newNodes, func(a, b int) bool { return len(s.newNodes[a].Pods) < len(s.newNodes[b].Pods) })

	// Pick existing node that we are about to create
	for _, node := range s.newNodes {
		if err := node.Add(ctx, pod); err == nil {
			return nil
		}
	}

	// Create new node
	var errs error
	for _, nodeTemplate := range s.machineTemplates {
		instanceTypes := s.instanceTypes[nodeTemplate.ProvisionerName]
		// if limits have been applied to the provisioner, ensure we filter instance types to avoid violating those limits
		if remaining, ok := s.remainingResources[nodeTemplate.ProvisionerName]; ok {
			instanceTypes = filterByRemainingResources(s.instanceTypes[nodeTemplate.ProvisionerName], remaining)
			if len(instanceTypes) == 0 {
				errs = multierr.Append(errs, fmt.Errorf("all available instance types exceed provisioner limits"))
				continue
			} else if len(s.instanceTypes[nodeTemplate.ProvisionerName]) != len(instanceTypes) && !s.opts.SimulationMode {
				logging.FromContext(ctx).Debugf("%d out of %d instance types were excluded because they would breach provisioner limits",
					len(s.instanceTypes[nodeTemplate.ProvisionerName])-len(instanceTypes), len(s.instanceTypes[nodeTemplate.ProvisionerName]))
			}
		}

		node := NewNode(nodeTemplate, s.topology, s.daemonOverhead[nodeTemplate], instanceTypes)
		if err := node.Add(ctx, pod); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("incompatible with provisioner %q, %w", nodeTemplate.ProvisionerName, err))
			continue
		}
		// we will launch this node and need to track its maximum possible resource usage against our remaining resources
		s.newNodes = append(s.newNodes, node)
		s.remainingResources[nodeTemplate.ProvisionerName] = subtractMax(s.remainingResources[nodeTemplate.ProvisionerName], node.InstanceTypeOptions)
		return nil
	}
	return errs
}

func (s *Scheduler) calculateExistingMachines(namedNodeTemplates map[string]*MachineTemplate, stateNodes []*state.Node) {
	// create our existing nodes
	for _, node := range stateNodes {
		name, ok := node.Node.Labels[v1alpha5.ProvisionerNameLabelKey]
		if !ok {
			// ignoring this node as it wasn't launched by us
			continue
		}
		nodeTemplate, ok := namedNodeTemplates[name]
		if !ok {
			// ignoring this node as it wasn't launched by a provisioner that we recognize
			continue
		}
		s.existingNodes = append(s.existingNodes, NewExistingNode(node, s.topology, nodeTemplate.StartupTaints, s.daemonOverhead[nodeTemplate]))

		// We don't use the status field and instead recompute the remaining resources to ensure we have a consistent view
		// of the cluster during scheduling.  Depending on how node creation falls out, this will also work for cases where
		// we don't create Node resources.
		s.remainingResources[name] = resources.Subtract(s.remainingResources[name], node.Capacity)
	}
}

// subtractMax returns the remaining resources after subtracting the max resource quantity per instance type. To avoid
// overshooting out, we need to pessimistically assume that if e.g. we request a 2, 4 or 8 CPU instance type
// that the 8 CPU instance type is all that will be available.  This could cause a batch of pods to take multiple rounds
// to schedule.
func subtractMax(remaining v1.ResourceList, instanceTypes []*cloudprovider.InstanceType) v1.ResourceList {
	// shouldn't occur, but to be safe
	if len(instanceTypes) == 0 {
		return remaining
	}
	var allInstanceResources []v1.ResourceList
	for _, it := range instanceTypes {
		allInstanceResources = append(allInstanceResources, it.Capacity)
	}
	result := v1.ResourceList{}
	itResources := resources.MaxResources(allInstanceResources...)
	for k, v := range remaining {
		cp := v.DeepCopy()
		cp.Sub(itResources[k])
		result[k] = cp
	}
	return result
}

// filterByRemainingResources is used to filter out instance types that if launched would exceed the provisioner limits
func filterByRemainingResources(instanceTypes []*cloudprovider.InstanceType, remaining v1.ResourceList) []*cloudprovider.InstanceType {
	var filtered []*cloudprovider.InstanceType
	for _, it := range instanceTypes {
		itResources := it.Capacity
		viableInstance := true
		for resourceName, remainingQuantity := range remaining {
			// if the instance capacity is greater than the remaining quantity for this resource
			if resources.Cmp(itResources[resourceName], remainingQuantity) > 0 {
				viableInstance = false
			}
		}
		if viableInstance {
			filtered = append(filtered, it)
		}
	}
	return filtered
}
