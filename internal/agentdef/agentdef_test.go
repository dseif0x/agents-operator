package agentdef

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	good := "---\nname: red-team\ndescription: Offensive security reviewer\ntools: Read, Grep, Bash\nmodel: glm-5.3-uncensored\n---\nYou review code for exploitable bugs.\n"
	if name, err := Parse(good); err != nil || name != "red-team" {
		t.Fatalf("Parse = %q, %v", name, err)
	}
	for in, want := range map[string]string{
		"You are an agent.": "starts with a front matter",
		"---\nname: x\ndescription: does things\nprompt":           "not closed",
		"---\nname: x\ndescription: does things\n---- \nbody":      "not closed",
		"---\nname: [x\n---\nbody":                                 "not valid YAML",
		"---\ndescription: does things\n---\nbody":                 "needs a name",
		"---\nname: Red Team\ndescription: does things\n---\nbody": "needs a name",
		"---\nname: ../evil\ndescription: does things\n---\nbody":  "needs a name",
		"---\nname: x\n---\nbody":                                  "needs a description",
		"---\nname: x\ndescription: does things\n---\n  \n":        "needs a prompt",
		"---\n" + strings.Repeat("a", MaxSize) + "\n---\nbody":     "limited",
	} {
		if _, err := Parse(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
