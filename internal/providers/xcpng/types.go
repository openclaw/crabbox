package xcpng

import "strings"

type xapiRef string

func (r xapiRef) value() string { return string(r) }

type xapiVM struct {
	Ref        string
	UUID       string
	Name       string
	PowerState string
	Labels     map[string]string
}

type xapiObject struct {
	Ref       string
	UUID      string
	NameLabel string
}

func xapiName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}
