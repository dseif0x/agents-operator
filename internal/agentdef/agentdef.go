// Package agentdef checks an agent definition for OpenClaude's --agent: a
// Markdown file whose YAML front matter names and describes the agent
// (optionally its tools, model and so on), with the agent's system prompt as
// the body, the same format as Claude Code's agent files.
package agentdef

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// MaxSize caps a definition.
const MaxSize = 32 << 10

// nameRE is what a name may look like here: it becomes a file name in
// ~/.openclaude/agents and a command-line argument.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Parse checks def and returns the agent's name.
func Parse(def string) (string, error) {
	if len(def) > MaxSize {
		return "", fmt.Errorf("an agent definition is limited to %d KiB", MaxSize>>10)
	}
	rest, ok := strings.CutPrefix(def, "---\n")
	if !ok {
		return "", errors.New("an agent definition starts with a front matter block between --- lines (name, description, optionally tools and model), followed by the agent's prompt")
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", errors.New("the agent definition's front matter is not closed with a --- line")
	}
	// The closing line is exactly "---".
	if body != "" && !strings.HasPrefix(body, "\n") {
		return "", errors.New("the agent definition's front matter is not closed with a --- line")
	}
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return "", fmt.Errorf("the agent definition's front matter is not valid YAML: %v", err)
	}
	name, _ := meta["name"].(string)
	if !nameRE.MatchString(name) {
		return "", errors.New("the agent definition needs a name: lowercase letters, digits and dashes, up to 64 characters")
	}
	if desc, _ := meta["description"].(string); strings.TrimSpace(desc) == "" {
		return "", errors.New("the agent definition needs a description")
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("the agent definition needs a prompt below its front matter")
	}
	return name, nil
}
