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
	"math"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/logging"

	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/functional"
)

const (
	InstanceTypesCacheKey              = "types"
	InstanceTypeZonesCacheKeyPrefix    = "zones:"
	InstanceTypesAndZonesCacheTTL      = 5 * time.Minute
	UnfulfillableCapacityErrorCacheTTL = 3 * time.Minute
)

type InstanceTypeProvider struct {
	sync.Mutex
	ec2api          ec2iface.EC2API
	subnetProvider  *SubnetProvider
	pricingProvider *PricingProvider
	// Has one cache entry for all the instance types (key: InstanceTypesCacheKey)
	// Has one cache entry for all the zones for each subnet selector (key: InstanceTypesZonesCacheKeyPrefix:<hash_of_selector>)
	// Values cached *before* considering insufficient capacity errors from the unavailableOfferings cache.
	cache *cache.Cache
	// key: <capacityType>:<instanceType>:<zone>, value: struct{}{}
	unavailableOfferings *cache.Cache
}

func NewInstanceTypeProvider(ec2api ec2iface.EC2API, subnetProvider *SubnetProvider, pricingProvider *PricingProvider) *InstanceTypeProvider {
	return &InstanceTypeProvider{
		ec2api:               ec2api,
		subnetProvider:       subnetProvider,
		pricingProvider:      pricingProvider,
		cache:                cache.New(InstanceTypesAndZonesCacheTTL, CacheCleanupInterval),
		unavailableOfferings: cache.New(UnfulfillableCapacityErrorCacheTTL, CacheCleanupInterval),
	}
}

// Get all instance type options
func (p *InstanceTypeProvider) Get(ctx context.Context, provider *v1alpha1.AWS) ([]cloudprovider.InstanceType, error) {
	p.Lock()
	defer p.Unlock()
	// Get InstanceTypes from EC2
	instanceTypes, err := p.getInstanceTypes(ctx, provider)
	if err != nil {
		return nil, err
	}
	// Get Viable EC2 Purchase offerings
	instanceTypeZones, err := p.getInstanceTypeZones(ctx, provider)
	if err != nil {
		return nil, err
	}
	var result []cloudprovider.InstanceType
	for _, i := range instanceTypes {
		// TODO: move pricing information from the instance type down into offerings
		instanceTypeName := aws.StringValue(i.InstanceType)
		price, err := p.pricingProvider.OnDemandPrice(instanceTypeName)
		if err != nil {
			// don't warn as this can occur extremely often
			price = math.MaxFloat64
		}
		instanceType := NewInstanceType(ctx, i, price, provider, p.createOfferings(i, instanceTypeZones[instanceTypeName]))
		result = append(result, instanceType)
	}
	return result, nil
}

func (p *InstanceTypeProvider) createOfferings(instanceType *ec2.InstanceTypeInfo, zones sets.String) []cloudprovider.Offering {
	offerings := []cloudprovider.Offering{}
	for zone := range zones {
		// while usage classes should be a distinct set, there's no guarantee of that
		for capacityType := range sets.NewString(aws.StringValueSlice(instanceType.SupportedUsageClasses)...) {
			// exclude any offerings that have recently seen an insufficient capacity error from EC2
			if _, isUnavailable := p.unavailableOfferings.Get(UnavailableOfferingsCacheKey(*instanceType.InstanceType, zone, capacityType)); !isUnavailable {
				offerings = append(offerings, cloudprovider.Offering{Zone: zone, CapacityType: capacityType})
			}
		}
	}
	return offerings
}

func (p *InstanceTypeProvider) getInstanceTypeZones(ctx context.Context, provider *v1alpha1.AWS) (map[string]sets.String, error) {
	subnetSelectorHash, err := hashstructure.Hash(provider.SubnetSelector, hashstructure.FormatV2, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to hash the subnet selector: %w", err)
	}
	cacheKey := fmt.Sprintf("%s%016x", InstanceTypeZonesCacheKeyPrefix, subnetSelectorHash)
	if cached, ok := p.cache.Get(cacheKey); ok {
		return cached.(map[string]sets.String), nil
	}

	// Constrain AZs from subnets
	subnets, err := p.subnetProvider.Get(ctx, provider)
	if err != nil {
		return nil, err
	}
	zones := sets.NewString(lo.Map(subnets, func(subnet *ec2.Subnet, _ int) string {
		return aws.StringValue(subnet.AvailabilityZone)
	})...)

	// Get offerings from EC2
	instanceTypeZones := map[string]sets.String{}
	if err := p.ec2api.DescribeInstanceTypeOfferingsPagesWithContext(ctx, &ec2.DescribeInstanceTypeOfferingsInput{LocationType: aws.String("availability-zone")},
		func(output *ec2.DescribeInstanceTypeOfferingsOutput, lastPage bool) bool {
			for _, offering := range output.InstanceTypeOfferings {
				if zones.Has(aws.StringValue(offering.Location)) {
					if _, ok := instanceTypeZones[aws.StringValue(offering.InstanceType)]; !ok {
						instanceTypeZones[aws.StringValue(offering.InstanceType)] = sets.NewString()
					}
					instanceTypeZones[aws.StringValue(offering.InstanceType)].Insert(aws.StringValue(offering.Location))
				}
			}
			return true
		}); err != nil {
		return nil, fmt.Errorf("describing instance type zone offerings, %w", err)
	}
	if _, ok := instanceTypeZones["p4de.24xlarge"]; !ok && zones.Has("us-east-1d") {
		logging.FromContext(ctx).Debugf("Forcing p4de.24xlarge in us-east-1d")
		instanceTypeZones["p4de.24xlarge"] = sets.NewString("us-east-1d")
	}
	logging.FromContext(ctx).Debugf("Discovered EC2 instance types zonal offerings (cache key: %v)", cacheKey)
	p.cache.SetDefault(cacheKey, instanceTypeZones)
	return instanceTypeZones, nil
}

// getInstanceTypes retrieves all instance types from the ec2 DescribeInstanceTypes API using some opinionated filters
func (p *InstanceTypeProvider) getInstanceTypes(ctx context.Context, provider *v1alpha1.AWS) (map[string]*ec2.InstanceTypeInfo, error) {
	if cached, ok := p.cache.Get(InstanceTypesCacheKey); ok {
		return cached.(map[string]*ec2.InstanceTypeInfo), nil
	}
	instanceTypes := map[string]*ec2.InstanceTypeInfo{}
	if err := p.ec2api.DescribeInstanceTypesPagesWithContext(ctx, &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("supported-virtualization-type"),
				Values: []*string{aws.String("hvm")},
			},
			{
				Name:   aws.String("processor-info.supported-architecture"),
				Values: aws.StringSlice([]string{"x86_64", "arm64"}),
			},
		},
	}, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
		for _, instanceType := range page.InstanceTypes {
			if p.filter(instanceType) {
				instanceTypes[aws.StringValue(instanceType.InstanceType)] = compressInstanceType(instanceType)
			}
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("fetching instance types using ec2.DescribeInstanceTypes, %w", err)
	}
	logging.FromContext(ctx).Debugf("Discovered %d EC2 instance types", len(instanceTypes))
	p.cache.SetDefault(InstanceTypesCacheKey, instanceTypes)
	return instanceTypes, nil
}

// filter the instance types to include useful ones for Kubernetes
func (p *InstanceTypeProvider) filter(instanceType *ec2.InstanceTypeInfo) bool {
	if instanceType.FpgaInfo != nil {
		return false
	}
	if functional.HasAnyPrefix(aws.StringValue(instanceType.InstanceType),
		// G2 instances have an older GPU not supported by the nvidia plugin. This causes the allocatable # of gpus
		// to be set to zero on startup as the plugin considers the GPU unhealthy.
		"g2",
	) {
		return false
	}
	return true
}

// CacheUnavailable allows the InstanceProvider to communicate recently observed temporary capacity shortages in
// the provided offerings
func (p *InstanceTypeProvider) CacheUnavailable(ctx context.Context, fleetErr *ec2.CreateFleetError, capacityType string) {
	instanceType := aws.StringValue(fleetErr.LaunchTemplateAndOverrides.Overrides.InstanceType)
	zone := aws.StringValue(fleetErr.LaunchTemplateAndOverrides.Overrides.AvailabilityZone)
	logging.FromContext(ctx).Debugf("%s for offering { instanceType: %s, zone: %s, capacityType: %s }, avoiding for %s",
		aws.StringValue(fleetErr.ErrorCode),
		instanceType,
		zone,
		capacityType,
		UnfulfillableCapacityErrorCacheTTL)
	// even if the key is already in the cache, we still need to call Set to extend the cached entry's TTL
	p.unavailableOfferings.SetDefault(UnavailableOfferingsCacheKey(instanceType, zone, capacityType), struct{}{})
}

func compressInstanceType(instanceType *ec2.InstanceTypeInfo) *ec2.InstanceTypeInfo {
	return &ec2.InstanceTypeInfo{
		InstanceType:             instanceType.InstanceType,
		Hypervisor:               instanceType.Hypervisor,
		SupportedUsageClasses:    instanceType.SupportedUsageClasses,
		VCpuInfo:                 &ec2.VCpuInfo{DefaultVCpus: instanceType.VCpuInfo.DefaultVCpus},
		GpuInfo:                  instanceType.GpuInfo,
		InferenceAcceleratorInfo: instanceType.InferenceAcceleratorInfo,
		InstanceStorageInfo:      instanceType.InstanceStorageInfo,
		MemoryInfo:               &ec2.MemoryInfo{SizeInMiB: instanceType.MemoryInfo.SizeInMiB},
		ProcessorInfo:            &ec2.ProcessorInfo{SupportedArchitectures: instanceType.ProcessorInfo.SupportedArchitectures},
		NetworkInfo: &ec2.NetworkInfo{
			Ipv4AddressesPerInterface: instanceType.NetworkInfo.Ipv4AddressesPerInterface,
			MaximumNetworkInterfaces:  instanceType.NetworkInfo.MaximumNetworkInterfaces,
		},
	}
}

func UnavailableOfferingsCacheKey(instanceType string, zone string, capacityType string) string {
	return fmt.Sprintf("%s:%s:%s", capacityType, instanceType, zone)
}
