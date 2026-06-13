package lambda

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

type Region struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type InstanceType struct {
	Name                         string   `json:"name"`
	Description                  string   `json:"description,omitempty"`
	RegionsWithCapacityAvailable []string `json:"regions_with_capacity_available,omitempty"`
}

type Image struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Family string `json:"family,omitempty"`
	Region string `json:"region,omitempty"`
}

type Instance struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Status      string   `json:"status,omitempty"`
	Region      Region   `json:"region,omitempty"`
	Type        string   `json:"instance_type_name,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	IP          string   `json:"ip,omitempty"`
	SSHKeyNames []string `json:"ssh_key_names,omitempty"`
}

type SSHKey struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key,omitempty"`
}

type Filesystem struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Region Region `json:"region,omitempty"`
}

type FirewallRuleset struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Region Region `json:"region,omitempty"`
}

type LaunchInstanceRequest struct {
	RegionName          string                   `json:"region_name"`
	InstanceTypeName    string                   `json:"instance_type_name"`
	SSHKeyNames         []string                 `json:"ssh_key_names"`
	Name                string                   `json:"name,omitempty"`
	ImageID             string                   `json:"image_id,omitempty"`
	ImageFamily         string                   `json:"image_family,omitempty"`
	UserData            string                   `json:"user_data,omitempty"`
	Tags                map[string]string        `json:"tags,omitempty"`
	FirewallRulesetName string                   `json:"firewall_ruleset_name,omitempty"`
	FileSystemNames     []string                 `json:"file_system_names,omitempty"`
	FileSystemMounts    []FilesystemMountRequest `json:"file_system_mounts,omitempty"`
}

type FilesystemMountRequest struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path,omitempty"`
}

type TerminateInstanceRequest struct {
	InstanceIDs []string `json:"instance_ids"`
}

type AddSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type DeleteSSHKeyRequest struct {
	ID string `json:"id"`
}

type UpdateInstanceNameRequest struct {
	Name string `json:"name"`
}
