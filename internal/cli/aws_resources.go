package cli

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Resource requirements use regional EC2 metadata, never the display catalog or
// an inferred instance-family size. Unconstrained allocation keeps its old path.
func (c *AWSClient) resourceQualifiedLaunchCandidates(ctx context.Context, cfg Config) ([]string, error) {
	if err := validateCapacityMinimumValues(cfg.Capacity); err != nil {
		return nil, err
	}
	candidates := AWSLaunchCandidates(cfg)
	if !hasCapacityMinimums(cfg) {
		return candidates, nil
	}
	if err := validateResourceRequirements(cfg); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no AWS launch candidates satisfy the requested resource requirements")
	}
	if c.ec2 == nil {
		return nil, fmt.Errorf("AWS instance resource metadata unavailable")
	}
	requested := make([]types.InstanceType, len(candidates))
	for i, candidate := range candidates {
		requested[i] = types.InstanceType(candidate)
	}
	metadata := make(map[string]types.InstanceTypeInfo, len(candidates))
	duplicate := make(map[string]bool)
	paginator := ec2.NewDescribeInstanceTypesPaginator(c.ec2, &ec2.DescribeInstanceTypesInput{InstanceTypes: requested})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("AWS instance resource metadata unavailable: %w", err)
		}
		for _, info := range page.InstanceTypes {
			name := string(info.InstanceType)
			if _, exists := metadata[name]; exists {
				duplicate[name] = true
			}
			metadata[name] = info
		}
	}
	qualified := make([]string, 0, len(candidates))
	unknown := 0
	for _, candidate := range candidates {
		info, found := metadata[candidate]
		if !found || duplicate[candidate] ||
			(cfg.Capacity.MinVCPUs > 0 && (info.VCpuInfo == nil || aws.ToInt32(info.VCpuInfo.DefaultVCpus) <= 0)) ||
			(cfg.Capacity.MinMemoryMiB > 0 && (info.MemoryInfo == nil || aws.ToInt64(info.MemoryInfo.SizeInMiB) <= 0)) {
			unknown++
			continue
		}
		if cfg.Capacity.MinVCPUs > 0 && int64(aws.ToInt32(info.VCpuInfo.DefaultVCpus)) < int64(cfg.Capacity.MinVCPUs) {
			continue
		}
		if cfg.Capacity.MinMemoryMiB > 0 && aws.ToInt64(info.MemoryInfo.SizeInMiB) < int64(cfg.Capacity.MinMemoryMiB) {
			continue
		}
		qualified = append(qualified, candidate)
	}
	if len(qualified) == 0 {
		if unknown > 0 {
			return nil, fmt.Errorf("no verified eligible AWS capacity: requested resource metadata is unavailable or invalid for %d candidate(s)", unknown)
		}
		return nil, fmt.Errorf("no AWS launch candidates satisfy minVCPUs=%d minMemoryMiB=%d", cfg.Capacity.MinVCPUs, cfg.Capacity.MinMemoryMiB)
	}
	return qualified, nil
}
