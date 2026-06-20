package scaleway

type Server struct {
	ID        string
	Name      string
	State     string
	PublicIP  string
	Zone      string
	Type      string
	Image     string
	Tags      []string
	ProjectID string
}

type SSHKey struct {
	ID          string
	Name        string
	PublicKey   string
	ProjectID   string
	Fingerprint string
}

type Image struct {
	ID    string
	Label string
	Name  string
	Zone  string
}

type ServerType struct {
	Name string
	Zone string
}
