// Package agentdef checks an agent definition for OpenCode's --agent: a
// Markdown file whose YAML front matter names and describes the agent
// (optionally OpenCode agent settings such as model, temperature or
// permission), with the agent's prompt as the body.
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

// nameRE is what a name may look like here: it becomes a key in OpenCode's
// agent configuration and a command-line argument.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Parse checks def and returns the agent's name.
func Parse(def string) (string, error) {
	d, err := ParseFull(def)
	return d.Name, err
}

// Definition is a parsed agent definition.
type Definition struct {
	Name string
	// Meta is the front matter, name included.
	Meta map[string]any
	// Prompt is the body below the front matter, trimmed.
	Prompt string
}

// ParseFull checks def and returns its parts.
func ParseFull(def string) (Definition, error) {
	if len(def) > MaxSize {
		return Definition{}, fmt.Errorf("an agent definition is limited to %d KiB", MaxSize>>10)
	}
	rest, ok := strings.CutPrefix(def, "---\n")
	if !ok {
		return Definition{}, errors.New("an agent definition starts with a front matter block between --- lines (name, description, optionally OpenCode agent settings such as model or temperature), followed by the agent's prompt")
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Definition{}, errors.New("the agent definition's front matter is not closed with a --- line")
	}
	// The closing line is exactly "---".
	if body != "" && !strings.HasPrefix(body, "\n") {
		return Definition{}, errors.New("the agent definition's front matter is not closed with a --- line")
	}
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return Definition{}, fmt.Errorf("the agent definition's front matter is not valid YAML: %v", err)
	}
	name, _ := meta["name"].(string)
	if !nameRE.MatchString(name) {
		return Definition{}, errors.New("the agent definition needs a name: lowercase letters, digits and dashes, up to 64 characters")
	}
	if desc, _ := meta["description"].(string); strings.TrimSpace(desc) == "" {
		return Definition{}, errors.New("the agent definition needs a description")
	}
	if strings.TrimSpace(body) == "" {
		return Definition{}, errors.New("the agent definition needs a prompt below its front matter")
	}
	return Definition{Name: name, Meta: meta, Prompt: strings.TrimSpace(body)}, nil
}
