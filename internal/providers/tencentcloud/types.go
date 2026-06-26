package tencentcloud

type getCallerIdentityResponse struct {
	AccountID string `json:"AccountId"`
	UIN       string `json:"Uin"`
	UserID    string `json:"UserId"`
	Arn       string `json:"Arn"`
	RequestID string `json:"RequestId"`
}

type describeInstancesResponse struct {
	TotalCount  int64      `json:"TotalCount"`
	InstanceSet []instance `json:"InstanceSet"`
	RequestID   string     `json:"RequestId"`
}

type runInstancesResponse struct {
	InstanceIDSet []string `json:"InstanceIdSet"`
	RequestID     string   `json:"RequestId"`
}

type instance struct {
	InstanceID         string    `json:"InstanceId"`
	InstanceName       string    `json:"InstanceName"`
	InstanceState      string    `json:"InstanceState"`
	InstanceType       string    `json:"InstanceType"`
	PublicIPAddresses  []string  `json:"PublicIpAddresses"`
	PrivateIPAddresses []string  `json:"PrivateIpAddresses"`
	Placement          placement `json:"Placement"`
	Tags               []tag     `json:"Tags"`
	CreatedTime        string    `json:"CreatedTime"`
}

type placement struct {
	Zone string `json:"Zone"`
}

type runInstanceRequest struct {
	InstanceChargeType  string               `json:"InstanceChargeType,omitempty"`
	Placement           placement            `json:"Placement"`
	ImageID             string               `json:"ImageId"`
	InstanceType        string               `json:"InstanceType"`
	SystemDisk          *systemDisk          `json:"SystemDisk,omitempty"`
	VirtualPrivateCloud *virtualPrivateCloud `json:"VirtualPrivateCloud,omitempty"`
	InternetAccessible  *internetAccessible  `json:"InternetAccessible,omitempty"`
	InstanceCount       int64                `json:"InstanceCount,omitempty"`
	InstanceName        string               `json:"InstanceName,omitempty"`
	SecurityGroupIDs    []string             `json:"SecurityGroupIds,omitempty"`
	UserData            string               `json:"UserData,omitempty"`
	TagSpecification    []tagSpecification   `json:"TagSpecification,omitempty"`
	ClientToken         string               `json:"ClientToken,omitempty"`
}

type systemDisk struct {
	DiskSize int64 `json:"DiskSize,omitempty"`
}

type virtualPrivateCloud struct {
	VPCID    string `json:"VpcId,omitempty"`
	SubnetID string `json:"SubnetId,omitempty"`
}

type internetAccessible struct {
	InternetChargeType      string `json:"InternetChargeType,omitempty"`
	InternetMaxBandwidthOut int64  `json:"InternetMaxBandwidthOut,omitempty"`
	PublicIPAssigned        bool   `json:"PublicIpAssigned"`
}

type tagSpecification struct {
	ResourceType string `json:"ResourceType"`
	Tags         []tag  `json:"Tags"`
}
