package nebius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type cliRunner struct {
	cfg NebiusConfig
	rt  Runtime
}

type cliResult struct {
	Stdout string
	Stderr string
}

var tokenLikePattern = regexp.MustCompile(`(?i)(osb_|ncp_|iam_token|oauth[_-]?token|access[_-]?token|refresh[_-]?token|private[_-]?key|BEGIN [A-Z ]*PRIVATE KEY)[A-Za-z0-9._:/+=@ -]*`)

func newCLIRunner(cfg NebiusConfig, rt Runtime) cliRunner {
	return cliRunner{cfg: cfg, rt: rt}
}

func (c cliRunner) run(ctx context.Context, args ...string) (cliResult, error) {
	commandArgs := c.withProfile(args)
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: c.cfg.CLI,
		Args: commandArgs,
	})
	if err != nil {
		return cliResult{Stdout: result.Stdout, Stderr: result.Stderr}, fmt.Errorf("nebius cli %s failed: %s", strings.Join(commandArgs, " "), redactNebiusText(firstNonBlank(result.Stderr, err.Error())))
	}
	return cliResult{Stdout: result.Stdout, Stderr: result.Stderr}, nil
}

func (c cliRunner) withProfile(args []string) []string {
	out := make([]string, 0, len(args)+2)
	if strings.TrimSpace(c.cfg.Profile) != "" {
		out = append(out, "--profile", strings.TrimSpace(c.cfg.Profile))
	}
	out = append(out, args...)
	return out
}

func parseJSONObject(output string) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	return object, nil
}

func parseJSONArray(output string) ([]map[string]any, error) {
	var objects []map[string]any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&objects); err != nil {
		return nil, err
	}
	return objects, nil
}

func stringField(object map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := object[name]; ok {
			switch typed := value.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					return typed
				}
			case json.Number:
				return typed.String()
			}
		}
	}
	return ""
}

func containsIDOrName(output, want string) (bool, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return false, nil
	}
	items, err := parseJSONArray(output)
	if err != nil {
		object, objectErr := parseJSONObject(output)
		if objectErr != nil {
			return false, err
		}
		items = []map[string]any{object}
	}
	for _, item := range items {
		if stringField(item, "id", "metadata.id") == want || stringField(item, "name", "family") == want {
			return true, nil
		}
	}
	return false, nil
}

func redactNebiusText(text string) string {
	text = tokenLikePattern.ReplaceAllString(text, "[REDACTED]")
	return strings.TrimSpace(text)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isJSON(output string) bool {
	var raw any
	return json.Unmarshal([]byte(output), &raw) == nil
}

func validationError(format string, args ...any) error {
	return exit(2, format, args...)
}

func notImplemented(action string) error {
	return errors.New("provider=nebius " + action + " is not implemented until Nebius lifecycle support lands")
}
