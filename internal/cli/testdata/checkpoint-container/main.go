// A secretless Docker command boundary for the built checkpoint CLI contract.
// It has no daemon, network client, shell execution, or host container access.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type state struct {
	Container map[string]any
	Removed   bool
	Image     string
	Tag       string
	Commits   int
	Removes   int
	Calls     [][]string
}

func main() {
	root := os.Getenv("CRABBOX_CONTAINER_FIXTURE")
	if root == "" || os.Getenv("DOCKER_HOST") != "unix://"+filepath.Join(root, "unused-docker.sock") || os.Getenv("DOCKER_CONFIG") != filepath.Join(root, "docker-config") {
		fail("captured Docker scope missing or changed")
	}
	path := filepath.Join(root, "docker.json")
	data, err := os.ReadFile(path)
	if err != nil {
		fail("read fixture: %v", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		fail("decode fixture: %v", err)
	}
	a := os.Args[1:]
	if len(a) == 0 {
		fail("missing command")
	}
	s.Calls = append(s.Calls, a)
	source, _ := s.Container["Id"].(string)
	const image = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var output string
	switch {
	case reflect.DeepEqual(a, []string{"info", "--format", "{{.ID}}"}):
		output = "fixture-daemon"
	case reflect.DeepEqual(a, []string{"context", "show"}):
		output = "default"
	case reflect.DeepEqual(a, []string{"context", "inspect", "default", "--format", `{{(index .Endpoints "docker").Host}}`}):
		output = "unix://" + filepath.Join(root, "unused-docker.sock")
	case reflect.DeepEqual(a, []string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}), reflect.DeepEqual(a, []string{"ps", "-a", "--no-trunc", "--format", "{{.ID}}"}):
		if !s.Removed {
			output = source
		}
	case reflect.DeepEqual(a, []string{"inspect", source}):
		if s.Removed {
			fail("No such container: %s", source)
		}
		value, _ := json.Marshal([]any{s.Container})
		output = string(value)
	case reflect.DeepEqual(a, []string{"inspect", source, "--format", "{{json .Mounts}}"}):
		output = "[]"
	case len(a) == 7 && reflect.DeepEqual(a[:6], []string{"exec", source, "sh", "-c", `cd "$1" && pwd -P`, "sh"}):
		output = a[6]
	case len(a) == 6 && a[0] == "commit" && a[1] == "--change" && strings.HasPrefix(a[2], "LABEL ") && a[3] == "--change" && strings.HasPrefix(a[4], "CMD ") && a[5] == source:
		if s.Removed {
			fail("commit reached removed source")
		}
		s.Commits++
		s.Image, output = image, image
	case len(a) == 3 && a[0] == "tag" && a[1] == s.Image && s.Image != "":
		s.Tag = a[2]
	case len(a) == 5 && a[0] == "image" && a[1] == "inspect" && a[2] == s.Tag && s.Tag != "" && a[3] == "--format" && a[4] == "{{.Id}}":
		output = s.Image
	case reflect.DeepEqual(a, []string{"rm", "-f", source}):
		if s.Image == "" || s.Tag == "" {
			fail("source retirement reached boundary before image was available")
		}
		s.Removes++
		s.Removed = true
	default:
		fail("unsupported fake Docker command: %q", a)
	}
	data, err = json.Marshal(s)
	if err != nil {
		fail("encode fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		fail("write fixture: %v", err)
	}
	if output != "" {
		fmt.Println(output)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
