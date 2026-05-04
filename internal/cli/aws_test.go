package cli

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestApplyAWSRunInstanceTargetOptionsEnablesNestedVirtualizationForWSL2(t *testing.T) {
	input := &ec2.RunInstancesInput{}
	applyAWSRunInstanceTargetOptions(input, Config{
		TargetOS:    targetWindows,
		WindowsMode: windowsModeWSL2,
	})
	if input.CpuOptions == nil {
		t.Fatal("CpuOptions=nil, want nested virtualization enabled")
	}
	if input.CpuOptions.NestedVirtualization != types.NestedVirtualizationSpecificationEnabled {
		t.Fatalf("NestedVirtualization=%q", input.CpuOptions.NestedVirtualization)
	}
}

func TestApplyAWSRunInstanceTargetOptionsLeavesNativeWindowsDefault(t *testing.T) {
	input := &ec2.RunInstancesInput{}
	applyAWSRunInstanceTargetOptions(input, Config{
		TargetOS:    targetWindows,
		WindowsMode: windowsModeNormal,
	})
	if input.CpuOptions != nil {
		t.Fatalf("CpuOptions=%#v, want nil", input.CpuOptions)
	}
}
