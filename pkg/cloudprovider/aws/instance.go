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

package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/options"
	"github.com/aws/karpenter/pkg/utils/resources"
)

const (
	// CreationQPS limits the number of requests per second to CreateFleet
	// https://docs.aws.amazon.com/AWSEC2/latest/APIReference/throttling.html#throttling-limits
	CreationQPS = 2
	// CreationBurst limits the additional burst requests.
	// https://docs.aws.amazon.com/AWSEC2/latest/APIReference/throttling.html#throttling-limits
	CreationBurst = 100
)

type InstanceProvider struct {
	ec2api                 ec2iface.EC2API
	instanceTypeProvider   *InstanceTypeProvider
	subnetProvider         *SubnetProvider
	launchTemplateProvider *LaunchTemplateProvider
}

func NewInstanceProvider(ec2api ec2iface.EC2API, instanceTypeProvider *InstanceTypeProvider, subnetProvider *SubnetProvider, launchTemplateProvider *LaunchTemplateProvider) *InstanceProvider {
	return &InstanceProvider{
		ec2api:                 ec2api,
		instanceTypeProvider:   instanceTypeProvider,
		subnetProvider:         subnetProvider,
		launchTemplateProvider: launchTemplateProvider,
	}
}

// Create an instance given the constraints.
// instanceTypes should be sorted by priority for spot capacity type.
// If spot is not used, the instanceTypes are not required to be sorted
// because we are using ec2 fleet's lowest-price OD allocation strategy
func (p *InstanceProvider) Create(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType) (*v1.Node, error) {
	instanceTypes = p.filterInstanceTypes(instanceTypes)
	if len(instanceTypes) > MaxInstanceTypes {
		instanceTypes = instanceTypes[0:MaxInstanceTypes]
	}

	id, err := p.launchInstance(ctx, constraints, instanceTypes)
	if err != nil {
		return nil, err
	}
	// Get Instance with backoff retry since EC2 is eventually consistent
	instance := &ec2.Instance{}
	if err := retry.Do(
		func() (err error) { instance, err = p.getInstance(ctx, aws.StringValue(id)); return err },
		retry.Delay(1*time.Second),
		retry.Attempts(6),
	); err != nil {
		return nil, err
	} else if err != nil {
		logging.FromContext(ctx).Errorf("retrieving node name for instance %s", aws.StringValue(instance.InstanceId))
	}
	logging.FromContext(ctx).Infof("Launched instance: %s, hostname: %s, type: %s, zone: %s, capacityType: %s",
		aws.StringValue(instance.InstanceId),
		aws.StringValue(instance.PrivateDnsName),
		aws.StringValue(instance.InstanceType),
		aws.StringValue(instance.Placement.AvailabilityZone),
		getCapacityType(instance),
	)
	// Convert Instance to Node
	return p.instanceToNode(ctx, instance, instanceTypes), nil
}

func (p *InstanceProvider) Terminate(ctx context.Context, node *v1.Node) error {
	id, err := getInstanceID(node)
	if err != nil {
		return fmt.Errorf("getting instance ID for node %s, %w", node.Name, err)
	}
	if _, err = p.ec2api.TerminateInstancesWithContext(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{id},
	}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("terminating instance %s, %w", node.Name, err)
	}
	return nil
}

func (p *InstanceProvider) launchInstance(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType) (*string, error) {
	capacityType := p.getCapacityType(constraints, instanceTypes)
	// Get Launch Template Configs, which may differ due to GPU or Architecture requirements
	launchTemplateConfigs, err := p.getLaunchTemplateConfigs(ctx, constraints, instanceTypes, capacityType)
	if err != nil {
		return nil, fmt.Errorf("getting launch template configs, %w", err)
	}
	// Create fleet
	tags := v1alpha1.MergeTags(ctx, constraints.Tags, map[string]string{fmt.Sprintf("kubernetes.io/cluster/%s", injection.GetOptions(ctx).ClusterName): "owned"})
	createFleetInput := &ec2.CreateFleetInput{
		Type:                  aws.String(ec2.FleetTypeInstant),
		LaunchTemplateConfigs: launchTemplateConfigs,
		TargetCapacitySpecification: &ec2.TargetCapacitySpecificationRequest{
			DefaultTargetCapacityType: aws.String(capacityType),
			TotalTargetCapacity:       aws.Int64(1),
		},
		TagSpecifications: []*ec2.TagSpecification{
			{ResourceType: aws.String(ec2.ResourceTypeInstance), Tags: tags},
			{ResourceType: aws.String(ec2.ResourceTypeVolume), Tags: tags},
		},
	}
	if capacityType == v1alpha1.CapacityTypeSpot {
		createFleetInput.SpotOptions = &ec2.SpotOptionsRequest{AllocationStrategy: aws.String(ec2.SpotAllocationStrategyCapacityOptimizedPrioritized)}
	} else {
		createFleetInput.OnDemandOptions = &ec2.OnDemandOptionsRequest{AllocationStrategy: aws.String(ec2.FleetOnDemandAllocationStrategyLowestPrice)}
	}
	createFleetOutput, err := p.ec2api.CreateFleetWithContext(ctx, createFleetInput)
	if err != nil {
		return nil, fmt.Errorf("creating fleet %w", err)
	}
	p.updateUnavailableOfferingsCache(ctx, createFleetOutput.Errors, capacityType)
	if len(createFleetOutput.Instances) == 0 || len(createFleetOutput.Instances[0].InstanceIds) == 0 {
		return nil, combineFleetErrors(createFleetOutput.Errors)
	}
	return createFleetOutput.Instances[0].InstanceIds[0], nil
}

func (p *InstanceProvider) getLaunchTemplateConfigs(ctx context.Context, constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType, capacityType string) ([]*ec2.FleetLaunchTemplateConfigRequest, error) {
	// Get subnets given the constraints
	subnets, err := p.subnetProvider.Get(ctx, constraints.AWS)
	if err != nil {
		return nil, fmt.Errorf("getting subnets, %w", err)
	}
	var launchTemplateConfigs []*ec2.FleetLaunchTemplateConfigRequest
	launchTemplates, err := p.launchTemplateProvider.Get(ctx, constraints, instanceTypes, map[string]string{v1alpha5.LabelCapacityType: capacityType})
	if err != nil {
		return nil, fmt.Errorf("getting launch templates, %w", err)
	}
	for launchTemplateName, instanceTypes := range launchTemplates {
		launchTemplateConfig := &ec2.FleetLaunchTemplateConfigRequest{
			Overrides: p.getOverrides(instanceTypes, subnets, constraints.Requirements.Zones(), capacityType),
			LaunchTemplateSpecification: &ec2.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateName: aws.String(launchTemplateName),
				Version:            aws.String("$Latest"),
			},
		}
		if len(launchTemplateConfig.Overrides) > 0 {
			launchTemplateConfigs = append(launchTemplateConfigs, launchTemplateConfig)
		}
	}
	if len(launchTemplateConfigs) == 0 {
		return nil, fmt.Errorf("no capacity offerings are currently available given the constraints")
	}
	return launchTemplateConfigs, nil
}

// getOverrides creates and returns launch template overrides for the cross product of instanceTypeOptions and subnets (with subnets being constrained by
// zones and the offerings in instanceTypeOptions)
func (p *InstanceProvider) getOverrides(instanceTypeOptions []cloudprovider.InstanceType, subnets []*ec2.Subnet, zones sets.String, capacityType string) []*ec2.FleetLaunchTemplateOverridesRequest {
	// sort subnets in ascending order of available IP addresses and populate map with most available subnet per AZ
	zonalSubnets := map[string]*ec2.Subnet{}
	sort.Slice(subnets, func(i, j int) bool {
		return aws.Int64Value(subnets[i].AvailableIpAddressCount) < aws.Int64Value(subnets[j].AvailableIpAddressCount)
	})
	for _, subnet := range subnets {
		zonalSubnets[*subnet.AvailabilityZone] = subnet
	}
	var overrides []*ec2.FleetLaunchTemplateOverridesRequest
	for i, instanceType := range instanceTypeOptions {
		for _, offering := range instanceType.Offerings() {
			if capacityType != offering.CapacityType {
				continue
			}
			if !zones.Has(offering.Zone) {
				continue
			}
			subnet, ok := zonalSubnets[offering.Zone]
			if !ok {
				continue
			}
			override := &ec2.FleetLaunchTemplateOverridesRequest{
				InstanceType: aws.String(instanceType.Name()),
				SubnetId:     subnet.SubnetId,
				// This is technically redundant, but is useful if we have to parse insufficient capacity errors from
				// CreateFleet so that we can figure out the zone rather than additional API calls to look up the subnet
				AvailabilityZone: subnet.AvailabilityZone,
			}
			// Add a priority for spot requests since we are using the capacity-optimized-prioritized spot allocation strategy
			// to reduce the likelihood of getting an excessively large instance type.
			// instanceTypeOptions are sorted by vcpus and memory so this prioritizes smaller instance types.
			if capacityType == v1alpha1.CapacityTypeSpot {
				override.Priority = aws.Float64(float64(i))
			}
			overrides = append(overrides, override)
		}
	}
	return overrides
}

func (p *InstanceProvider) getInstance(ctx context.Context, id string) (*ec2.Instance, error) {
	describeInstancesOutput, err := p.ec2api.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{InstanceIds: aws.StringSlice([]string{id})})
	if isNotFound(err) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("failed to describe ec2 instances, %w", err)
	}
	if len(describeInstancesOutput.Reservations) != 1 || len(describeInstancesOutput.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("expected instance but got 0")
	}
	instance := describeInstancesOutput.Reservations[0].Instances[0]
	if injection.GetOptions(ctx).GetAWSNodeNameConvention() == options.ResourceName {
		return instance, nil
	}
	if len(aws.StringValue(instance.PrivateDnsName)) == 0 {
		return nil, multierr.Append(err, fmt.Errorf("got instance %s but PrivateDnsName was not set", aws.StringValue(instance.InstanceId)))
	}
	return instance, nil
}

func (p *InstanceProvider) instanceToNode(ctx context.Context, instance *ec2.Instance, instanceTypes []cloudprovider.InstanceType) *v1.Node {
	for _, instanceType := range instanceTypes {
		if instanceType.Name() == aws.StringValue(instance.InstanceType) {
			nodeName := strings.ToLower(aws.StringValue(instance.PrivateDnsName))
			if injection.GetOptions(ctx).GetAWSNodeNameConvention() == options.ResourceName {
				nodeName = aws.StringValue(instance.InstanceId)
			}

			resources := v1.ResourceList{}
			for resourceName, quantity := range map[v1.ResourceName]resource.Quantity{
				v1.ResourcePods:             instanceType.Resources()[v1.ResourcePods],
				v1.ResourceCPU:              instanceType.Resources()[v1.ResourceCPU],
				v1.ResourceMemory:           instanceType.Resources()[v1.ResourceMemory],
				v1.ResourceEphemeralStorage: instanceType.Resources()[v1.ResourceEphemeralStorage],
				v1alpha1.ResourceNVIDIAGPU:  instanceType.Resources()[v1alpha1.ResourceNVIDIAGPU],
				v1alpha1.ResourceAMDGPU:     instanceType.Resources()[v1alpha1.ResourceAMDGPU],
				v1alpha1.ResourceAWSNeuron:  instanceType.Resources()[v1alpha1.ResourceAWSNeuron],
			} {
				if !quantity.IsZero() {
					resources[resourceName] = quantity
				}
			}

			return &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						v1.LabelTopologyZone:       aws.StringValue(instance.Placement.AvailabilityZone),
						v1.LabelInstanceTypeStable: aws.StringValue(instance.InstanceType),
						v1alpha5.LabelCapacityType: getCapacityType(instance),
					},
				},
				Spec: v1.NodeSpec{
					ProviderID: fmt.Sprintf("aws:///%s/%s", aws.StringValue(instance.Placement.AvailabilityZone), aws.StringValue(instance.InstanceId)),
				},
				Status: v1.NodeStatus{
					Allocatable: resources,
					Capacity:    resources,
					NodeInfo: v1.NodeSystemInfo{
						Architecture:    v1alpha1.AWSToKubeArchitectures[aws.StringValue(instance.Architecture)],
						OSImage:         aws.StringValue(instance.ImageId),
						OperatingSystem: v1alpha5.OperatingSystemLinux,
					},
				},
			}
		}
	}
	panic(fmt.Sprintf("unrecognized instance type %s", aws.StringValue(instance.InstanceType)))
}

func (p *InstanceProvider) updateUnavailableOfferingsCache(ctx context.Context, errors []*ec2.CreateFleetError, capacityType string) {
	for _, err := range errors {
		if InsufficientCapacityErrorCode == aws.StringValue(err.ErrorCode) {
			p.instanceTypeProvider.CacheUnavailable(ctx, aws.StringValue(err.LaunchTemplateAndOverrides.Overrides.InstanceType), aws.StringValue(err.LaunchTemplateAndOverrides.Overrides.AvailabilityZone), capacityType)
		}
	}
}

// getCapacityType selects spot if both constraints are flexible and there is an
// available offering. The AWS Cloud Provider defaults to [ on-demand ], so spot
// must be explicitly included in capacity type requirements.
func (p *InstanceProvider) getCapacityType(constraints *v1alpha1.Constraints, instanceTypes []cloudprovider.InstanceType) string {
	if constraints.Requirements.CapacityTypes().Has(v1alpha1.CapacityTypeSpot) {
		for _, instanceType := range instanceTypes {
			for _, offering := range instanceType.Offerings() {
				if constraints.Requirements.Zones().Has(offering.Zone) && offering.CapacityType == v1alpha1.CapacityTypeSpot {
					return v1alpha1.CapacityTypeSpot
				}
			}
		}
	}
	return v1alpha1.CapacityTypeOnDemand
}

// filterInstanceTypes is used to eliminate GPU instance types from the list of possible instance types when a
// non-GPU instance type will work.  If the list of instance types consists of both GPU and non-GPU types, then only
// the non-GPU types will be returned.  If it has only GPU types, the list will be returned unaltered.
func (p *InstanceProvider) filterInstanceTypes(instanceTypes []cloudprovider.InstanceType) []cloudprovider.InstanceType {
	var genericInstanceTypes []cloudprovider.InstanceType
	for _, it := range instanceTypes {
		itRes := it.Resources()
		if resources.IsZero(itRes[v1alpha1.ResourceAWSNeuron]) &&
			resources.IsZero(itRes[v1alpha1.ResourceAMDGPU]) &&
			resources.IsZero(itRes[v1alpha1.ResourceNVIDIAGPU]) {
			genericInstanceTypes = append(genericInstanceTypes, it)
		}
	}
	// if we got some subset of non-GPU types, then prefer to use those
	if len(genericInstanceTypes) != 0 {
		return genericInstanceTypes
	}
	return instanceTypes
}

func getInstanceID(node *v1.Node) (*string, error) {
	id := strings.Split(node.Spec.ProviderID, "/")
	if len(id) < 5 {
		return nil, fmt.Errorf("parsing instance id %s", node.Spec.ProviderID)
	}
	return aws.String(id[4]), nil
}

func combineFleetErrors(errors []*ec2.CreateFleetError) (errs error) {
	unique := sets.NewString()
	for _, err := range errors {
		unique.Insert(fmt.Sprintf("%s: %s", aws.StringValue(err.ErrorCode), aws.StringValue(err.ErrorMessage)))
	}
	for errorCode := range unique {
		errs = multierr.Append(errs, fmt.Errorf(errorCode))
	}
	return fmt.Errorf("with fleet error(s), %w", errs)
}

func getCapacityType(instance *ec2.Instance) string {
	if instance.SpotInstanceRequestId != nil {
		return v1alpha1.CapacityTypeSpot
	}
	return v1alpha1.CapacityTypeOnDemand
}
