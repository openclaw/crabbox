package ssh

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticConfigShowRawStrings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config core.StaticConfig
		values [6]string
		text   string
	}{
		{
			name: "empty",
			text: "static id=- name=- host=- user=- port=- work_root=-\n",
		},
		{
			name:   "configured and leading-zero port",
			config: core.StaticConfig{ID: "box-id", Name: "box-name", Host: "offline.example.test", User: "alice", Port: "0022", WorkRoot: " /srv/raw "},
			values: [6]string{"box-id", "box-name", "offline.example.test", "alice", "0022", " /srv/raw "},
			text:   "static id=box-id name=box-name host=offline.example.test user=alice port=0022 work_root= /srv/raw \n",
		},
		{
			name:   "whitespace is not empty",
			config: core.StaticConfig{ID: " \t ", Name: " \t ", Host: " \t ", User: " \t ", Port: " \t ", WorkRoot: " \t "},
			values: [6]string{" \t ", " \t ", " \t ", " \t ", " \t ", " \t "},
			text:   "static id= \t  name= \t  host= \t  user= \t  port= \t  work_root= \t \n",
		},
		{
			name:   "no ID-host fallback or numeric port conversion",
			config: core.StaticConfig{ID: "only-id", Port: "0"},
			values: [6]string{"only-id", "", "", "", "0", ""},
			text:   "static id=only-id name=- host=- user=- port=0 work_root=-\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, selection := range []string{"ssh", "static", "static-ssh", "unselected-display"} {
				cfg := core.Config{Provider: selection, Static: tc.config, SSHUser: "generic-user", SSHPort: "9999", WorkRoot: "/generic"}
				before := cfg
				section := (Provider{}).ConfigShowSection(cfg)
				if section.JSONKey != "static" || section.TextLabel != "static" || !reflect.DeepEqual(section.Providers, []string{"ssh"}) {
					t.Fatalf("selection=%s section identity=%#v", selection, section)
				}
				jsonNames := []string{"id", "name", "host", "user", "port", "workRoot"}
				textNames := []string{"id", "name", "host", "user", "port", "work_root"}
				if len(section.Fields) != len(jsonNames) {
					t.Fatalf("fields=%d want 6", len(section.Fields))
				}
				values := make(map[string]any, len(jsonNames))
				want := make(map[string]string, len(jsonNames))
				var text strings.Builder
				text.WriteString(section.TextLabel)
				for i, field := range section.Fields {
					if field.JSONName != jsonNames[i] || field.TextName != textNames[i] || field.JSONValue != tc.values[i] {
						t.Fatalf("field %d=%#v want %s/%s raw string %q", i, field, jsonNames[i], textNames[i], tc.values[i])
					}
					values[field.JSONName] = field.JSONValue
					want[field.JSONName] = tc.values[i]
					text.WriteString(" " + field.TextName + "=" + field.TextValue)
				}
				text.WriteByte('\n')
				if text.String() != tc.text {
					t.Fatalf("text=%q want %q", text.String(), tc.text)
				}
				encoded, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string]string
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatalf("JSON fields must remain strings: %v", err)
				}
				if !reflect.DeepEqual(decoded, want) || !reflect.DeepEqual(cfg, before) {
					t.Fatal("raw JSON values or source configuration changed")
				}
			}
		})
	}
}
