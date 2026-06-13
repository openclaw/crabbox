package linode

type account struct {
	EUUID string `json:"euuid"`
}

type accountSettings struct {
	InterfacesForNewLinodes string `json:"interfaces_for_new_linodes"`
}

type linodeInstance struct {
	ID      int64    `json:"id"`
	Label   string   `json:"label"`
	Status  string   `json:"status"`
	Region  string   `json:"region"`
	Type    string   `json:"type"`
	Image   string   `json:"image"`
	Tags    []string `json:"tags"`
	IPv4    []string `json:"ipv4"`
	IPv6    string   `json:"ipv6"`
	Group   string   `json:"group"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
}

type createLinodeRequest struct {
	Region              string            `json:"region"`
	Type                string            `json:"type"`
	Image               string            `json:"image"`
	Label               string            `json:"label,omitempty"`
	Tags                []string          `json:"tags,omitempty"`
	AuthorizedKeys      []string          `json:"authorized_keys,omitempty"`
	RootPass            string            `json:"root_pass,omitempty"`
	Metadata            *linodeMetadata   `json:"metadata,omitempty"`
	FirewallID          int64             `json:"firewall_id,omitempty"`
	InterfaceGeneration string            `json:"interface_generation,omitempty"`
	Interfaces          []linodeInterface `json:"interfaces,omitempty"`
	PrivateIP           bool              `json:"private_ip,omitempty"`
	StackScriptID       int64             `json:"stackscript_id,omitempty"`
	StackScriptData     map[string]string `json:"stackscript_data,omitempty"`
}

type linodeMetadata struct {
	UserData string `json:"user_data,omitempty"`
}

type linodeInterface struct {
	Purpose    string    `json:"purpose,omitempty"`
	Label      string    `json:"label,omitempty"`
	FirewallID *int64    `json:"firewall_id,omitempty"`
	Public     *struct{} `json:"public,omitempty"`
}

type linodeType struct {
	ID      string      `json:"id"`
	Label   string      `json:"label"`
	Class   string      `json:"class"`
	Memory  int         `json:"memory"`
	Disk    int         `json:"disk"`
	VCPUs   int         `json:"vcpus"`
	Network int         `json:"network_out"`
	Price   linodePrice `json:"price"`
	Addons  any         `json:"addons,omitempty"`
}

type linodePrice struct {
	Hourly  float64 `json:"hourly"`
	Monthly float64 `json:"monthly"`
}

type linodeImage struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Vendor      string   `json:"vendor"`
	Deprecated  bool     `json:"deprecated"`
	IsPublic    bool     `json:"is_public"`
	Regions     []string `json:"regions"`
	Description string   `json:"description"`
}

type linodeRegion struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Country      string   `json:"country"`
	Status       string   `json:"status"`
	SiteType     string   `json:"site_type"`
	Capabilities []string `json:"capabilities"`
}

type linodeFirewall struct {
	ID      int64    `json:"id"`
	Label   string   `json:"label"`
	Status  string   `json:"status"`
	Tags    []string `json:"tags"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
}

type firewallDeviceRequest struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
