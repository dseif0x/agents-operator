package reconcile

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func testCfg() Config {
	return Config{
		Namespace: "agents-operator", RunnerImage: "ghcr.io/x/agents-operator-runner", RunnerImageTag: "1.0.0",
		DefaultStorageClass: "nfs-fast", DefaultPVCSize: "20Gi",
		DefaultResources: config.Resources{
			Requests: config.ResourceList{CPU: "250m", Memory: "512Mi"},
			Limits:   config.ResourceList{CPU: "2", Memory: "4Gi"},
		},
		NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
		Tolerations:  []config.Toleration{{Key: "arm", Operator: "Exists"}},
		RuntimeClass: "gvisor",
		ExtraEnv:     map[string]string{"ANTHROPIC_BASE_URL": "http://cliproxy"},
	}.Defaults()
}

func testSession() *store.Session {
	return &store.Session{
		ID: "11111111-2222-4333-8444-555555555555", OwnerID: "u1", Name: "My Session!", Agent: "claude",
		Repos:      []store.Repo{{URL: "git@github.com:x/y.git", Branch: "main", Path: "y"}, {URL: "https://github.com/x/z.git", Path: "z"}},
		Autonomous: true, Generation: 3,
		Env:          map[string]string{"FOO": "bar"},
		NodeSelector: map[string]string{"kubernetes.io/arch": "arm64"},
	}
}

func TestBuildPVC(t *testing.T) {
	s := testSession()
	pvc := BuildPVC(s, testCfg())
	if pvc.Name != "agents-operator-"+s.ID || *pvc.Spec.StorageClassName != "nfs-fast" {
		t.Fatalf("pvc = %+v", pvc)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != "20Gi" || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("pvc spec = %+v", pvc.Spec)
	}
	s.PVCSize, s.StorageClass = "5Gi", "nfs"
	pvc = BuildPVC(s, testCfg())
	if pvc.Spec.Resources.Requests.Storage().String() != "5Gi" || *pvc.Spec.StorageClassName != "nfs" {
		t.Fatalf("override ignored: %+v", pvc.Spec)
	}
	if pvc.Labels[k8s.LabelSession] != s.ID || pvc.Labels[k8s.LabelOwner] != "u1" {
		t.Fatalf("labels = %v", pvc.Labels)
	}
}

// testCredentials is an owner with one account of every kind (two Claude
// Code ones), each value "v-<id>".
func testCredentials() UserCredentials {
	acct := func(id, kind, name string) *store.Account {
		return &store.Account{ID: id, UserID: "u1", Kind: kind, Name: name}
	}
	accounts := []*store.Account{
		{ID: "ant", UserID: "u1", Kind: store.AcctAnthropicAPIKey, Name: "Anthropic", BaseURL: "https://proxy"},
		acct("oat", store.AcctClaudeOAuthToken, "Max plan"),
		acct("cl", store.AcctClaudeLogin, "Claude login"),
		acct("oai", store.AcctOpenAIAPIKey, "OpenAI"),
		acct("cx", store.AcctCodexLogin, "Codex login"),
		{ID: "llm", UserID: "u1", Kind: store.AcctOpenAICompatible, Name: "Work LiteLLM", BaseURL: "http://litellm/v1", Models: []string{"m"}},
		acct("uj", store.AcctUnjailed, "Unjailed"),
		{ID: "gh", UserID: "u1", Kind: store.AcctGitHTTPS, Name: "github.com", Host: "github.com", Username: ""},
		{ID: "any", UserID: "u1", Kind: store.AcctGitHTTPS, Name: "*", Host: "*", Username: "bob"},
		{ID: "ssh", UserID: "u1", Kind: store.AcctGitSSH, Name: "*", Host: "*"},
		acct("empty", store.AcctOpenAIAPIKey, "No value"),
	}
	values := map[string][]byte{"unrelated": []byte("x")}
	for _, a := range accounts {
		if a.ID != "empty" {
			values[store.AccountSecretKey(a.ID)] = []byte("v-" + a.ID)
		}
	}
	return UserCredentials{Accounts: accounts, Values: values, Profile: &store.Profile{UserID: "u1", GitEmail: "a@b.c", Defaults: map[string]string{runner.AgentClaude: "oat"}}}
}

func TestBuildSecret(t *testing.T) {
	s := testSession() // claude
	sec := BuildSecret(s, testCfg(), "tok", testCredentials())
	d := sec.Data
	// The default Claude account (the OAuth token), git and the identity.
	if string(d["RUNNER_TOKEN"]) != "tok" || string(d["CLAUDE_CODE_OAUTH_TOKEN"]) != "v-oat" || string(d["GH_TOKEN"]) != "v-gh" || string(d["GIT_USER_EMAIL"]) != "a@b.c" {
		t.Fatalf("data = %v", d)
	}
	var https []runner.GitHTTPSCredential
	if err := json.Unmarshal(d[runner.EnvGitHTTPS], &https); err != nil || len(https) != 2 || https[1] != (runner.GitHTTPSCredential{Host: "*", Username: "bob", Token: "v-any"}) {
		t.Fatalf("https = %+v, %v", https, err)
	}
	var ssh []runner.GitSSHKey
	if err := json.Unmarshal(d[runner.EnvGitSSH], &ssh); err != nil || len(ssh) != 1 || ssh[0].Key != "v-ssh" {
		t.Fatalf("ssh = %+v, %v", ssh, err)
	}
	if _, ok := d["unrelated"]; ok {
		t.Fatal("unknown key projected")
	}
	if sec.Annotations[k8s.AnnotationGeneration] != "3" {
		t.Fatalf("generation annotation = %v", sec.Annotations)
	}
	// A pick of its own wins over the default: the saved login, seeded.
	s.AIAccount = "cl"
	d = BuildSecret(s, testCfg(), "tok", testCredentials()).Data
	if string(d["AGENTS_OPERATOR_LOGIN_CLAUDE_LOGIN"]) != base64.StdEncoding.EncodeToString([]byte("v-cl")) || string(d["AGENTS_OPERATOR_SEED_ACCOUNT_CLAUDE_LOGIN"]) != "cl" {
		t.Fatalf("login seed = %v", d)
	}
	if _, ok := d["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("the default account projected next to the picked one")
	}
	// No user secret at all still yields a token.
	sec = BuildSecret(s, testCfg(), "tok2", UserCredentials{})
	if len(sec.Data) != 1 || string(sec.Data["RUNNER_TOKEN"]) != "tok2" {
		t.Fatalf("data = %v", sec.Data)
	}
}

func TestBuildSecretScopesAIAccounts(t *testing.T) {
	git := []string{"GH_TOKEN", runner.EnvGitHTTPS, runner.EnvGitSSH, "GIT_USER_EMAIL"}
	keys := func(names ...string) []string {
		out := []string{runner.EnvAIAccounts}
		for _, n := range names {
			out = append(out, runner.EnvAIKeyPrefix+n)
		}
		return out
	}
	for agent, want := range map[string][]string{
		runner.AgentClaude: {"CLAUDE_CODE_OAUTH_TOKEN"},
		runner.AgentCodex:  {"OPENAI_API_KEY"}, // no default: the oldest account Codex can use
		// Every provider account, each key under the account's name.
		runner.AgentOpenCode: keys("ANTHROPIC", "OPENAI", "WORK_LITELLM", "UNJAILED"),
		// The defaults in the CLIs' variables, plus every account with a key.
		runner.AgentShell: append([]string{"CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY"},
			keys("ANTHROPIC", "MAX_PLAN", "OPENAI", "WORK_LITELLM", "UNJAILED")...),
		"some-future-agent": nil,
	} {
		s := testSession()
		s.Agent = agent
		data := BuildSecret(s, testCfg(), "tok", testCredentials()).Data
		want = append(append([]string{"RUNNER_TOKEN"}, git...), want...)
		got := make([]string, 0, len(data))
		for k := range data {
			got = append(got, k)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: keys = %v, want %v", agent, got, want)
		}
	}

	// The OpenCode description names each key's variable, never a value.
	s := testSession()
	s.Agent = runner.AgentOpenCode
	data := BuildSecret(s, testCfg(), "tok", testCredentials()).Data
	var described []runner.AIAccount
	if err := json.Unmarshal(data[runner.EnvAIAccounts], &described); err != nil {
		t.Fatal(err)
	}
	if len(described) != 4 || !reflect.DeepEqual(described[2], runner.AIAccount{ID: "llm", Name: "Work LiteLLM", Kind: store.AcctOpenAICompatible, BaseURL: "http://litellm/v1", Models: []string{"m"}, KeyEnv: runner.EnvAIKeyPrefix + "WORK_LITELLM"}) {
		t.Fatalf("described = %+v", described)
	}
	if strings.Contains(string(data[runner.EnvAIAccounts]), "v-") {
		t.Fatal("a key in the description")
	}
}

func TestSelectAccount(t *testing.T) {
	uc := testCredentials()
	cases := []struct{ agent, pick, want string }{
		{runner.AgentClaude, "", "oat"},     // the default
		{runner.AgentClaude, "cl", "cl"},    // the session's pick
		{runner.AgentClaude, "oai", "oat"},  // not a Claude account: the default
		{runner.AgentClaude, "gone", "oat"}, // deleted: the default
		{runner.AgentCodex, "", "oai"},      // no default: the oldest
		{runner.AgentCodex, "cx", "cx"},
		{runner.AgentOpenCode, "ant", ""}, // OpenCode signs in through providers
	}
	for _, c := range cases {
		got := ""
		if a := SelectAccount(c.agent, c.pick, uc.Accounts, uc.Profile); a != nil {
			got = a.ID
		}
		if got != c.want {
			t.Errorf("%s pick %q = %q, want %q", c.agent, c.pick, got, c.want)
		}
	}
}

func TestKeyEnv(t *testing.T) {
	used := map[string]bool{}
	for _, c := range [][2]string{{"Work LiteLLM", "WORK_LITELLM"}, {"work-litellm", "WORK_LITELLM_2"}, {"äöü", "ACCOUNT"}, {" ", "ACCOUNT_2"}} {
		if got := keyEnv(c[0], used); got != runner.EnvAIKeyPrefix+c[1] {
			t.Errorf("%q = %s", c[0], got)
		}
	}
}

func TestClampResources(t *testing.T) {
	def := testCfg().DefaultResources
	// No overrides: defaults.
	rr := ClampResources(config.Resources{}, def, config.Resources{})
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" || rr.Requests.Cpu().String() != "250m" {
		t.Fatalf("defaults: %+v", rr)
	}
	// Lowering is allowed.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "500m", Memory: "1Gi"}}, def, config.Resources{})
	if rr.Limits.Cpu().String() != "500m" || rr.Limits.Memory().String() != "1Gi" {
		t.Fatalf("lower: %+v", rr)
	}
	// Raising is clamped to the default, and requests never exceed limits.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "8", Memory: "64Gi"}, Requests: config.ResourceList{CPU: "4"}}, def, config.Resources{})
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" || rr.Requests.Cpu().String() != "2" {
		t.Fatalf("raise: %+v", rr)
	}
	// Request above a lowered limit is pulled down.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{Memory: "256Mi"}}, def, config.Resources{})
	if rr.Requests.Memory().String() != "256Mi" {
		t.Fatalf("request > limit: %+v", rr)
	}
}

func TestHostname(t *testing.T) {
	cases := map[string]string{"My Session!": "my-session", "": "session", "--x--": "x", "ok-name": "ok-name"}
	for in, want := range cases {
		if got := Hostname(in); got != want {
			t.Errorf("Hostname(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBuildPod(t *testing.T) {
	s := testSession()
	pod := BuildPod(s, testCfg())
	if pod.Name != ObjectName(s.ID) || pod.Namespace != "agents-operator" || pod.Spec.Hostname != "my-session" {
		t.Fatalf("meta: %+v", pod.ObjectMeta)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.AutomountServiceAccountToken || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("spec: %+v", pod.Spec)
	}
	psc := pod.Spec.SecurityContext
	if !*psc.RunAsNonRoot || *psc.RunAsUser != 1000 || *psc.RunAsGroup != 1000 || *psc.FSGroup != 1000 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context: %+v", psc)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatal("one container expected")
	}
	c := pod.Spec.Containers[0]
	if c.Image != "ghcr.io/x/agents-operator-runner:1.0.0" {
		t.Fatalf("image = %s", c.Image)
	}
	csc := c.SecurityContext
	if *csc.AllowPrivilegeEscalation || !*csc.ReadOnlyRootFilesystem || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security context: %+v", csc)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Fatal("host namespaces must be off")
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatal("hostPath volume present")
		}
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["AGENT"] != "claude" || env["AUTONOMOUS"] != "true" || env["AGENT_GIT_CREDENTIALS"] != "true" || env["FOO"] != "bar" || env["ANTHROPIC_BASE_URL"] != "http://cliproxy" {
		t.Fatalf("env = %v", env)
	}
	if env["REPOS"] != `[{"url":"git@github.com:x/y.git","branch":"main","path":"y"},{"url":"https://github.com/x/z.git","path":"z"}]` {
		t.Fatalf("REPOS = %s", env["REPOS"])
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != ObjectName(s.ID) {
		t.Fatalf("envFrom = %+v", c.EnvFrom)
	}
	if c.Ports[0].ContainerPort != 7681 || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("ports/probes: %+v", c)
	}
	if pod.Spec.NodeSelector["kubernetes.io/os"] != "linux" || pod.Spec.NodeSelector["kubernetes.io/arch"] != "arm64" {
		t.Fatalf("nodeSelector = %v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "arm" {
		t.Fatalf("tolerations = %v", pod.Spec.Tolerations)
	}
	if c.Resources.Limits.Cpu().String() != "2" {
		t.Fatalf("resources = %+v", c.Resources)
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["workspace"] != "/workspace" || mounts["tmp"] != "/tmp" {
		t.Fatalf("mounts = %v", mounts)
	}
	s.ImageTag = "dev"
	if BuildPod(s, testCfg()).Spec.Containers[0].Image != "ghcr.io/x/agents-operator-runner:dev" {
		t.Fatal("image tag override ignored")
	}
}

func TestPodHelpers(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if !PodReady(pod) || PodTerminal(pod) {
		t.Fatal("ready pod misclassified")
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}}}}
	if !PodTerminal(pod) || PodReason(pod) != "container exited with code 2 (Error)" {
		t.Fatalf("terminal: %v %q", PodTerminal(pod), PodReason(pod))
	}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "no nodes"}}}}
	if PodReason(pending) != "Unschedulable: no nodes" {
		t.Fatalf("reason = %q", PodReason(pending))
	}
	if PodReason(nil) != "pod missing" {
		t.Fatal("nil reason")
	}
}

func TestBuildPodTmpInit(t *testing.T) {
	s := testSession()
	cfg := testCfg()
	cfg.TmpInit = true
	pod := BuildPod(s, cfg)
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %+v", pod.Spec.InitContainers)
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Image != pod.Spec.Containers[0].Image || len(ic.Command) != 3 || ic.Command[0] != "chmod" || ic.Command[1] != "1777" || ic.Command[2] != "/tmp" {
		t.Fatalf("init container = %+v", ic)
	}
	if len(ic.VolumeMounts) != 1 || ic.VolumeMounts[0].Name != "tmp" || ic.VolumeMounts[0].MountPath != "/tmp" {
		t.Fatalf("init mounts = %+v", ic.VolumeMounts)
	}
	sc := ic.SecurityContext
	if *sc.RunAsUser != 0 || *sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("init security context = %+v", sc)
	}
	// The runner itself stays non-root.
	if !*pod.Spec.Containers[0].SecurityContext.RunAsNonRoot || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("runner container lost its non-root setting")
	}
	cfg.TmpInit = false
	if pod := BuildPod(s, cfg); len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("init container present when disabled: %+v", pod.Spec.InitContainers)
	}
}

func TestBuildPodRuntimeClass(t *testing.T) {
	s := testSession()
	cfg := testCfg() // chart-wide gvisor
	s.RuntimeClass = "kata"
	if pod := BuildPod(s, cfg); *pod.Spec.RuntimeClassName != "kata" {
		t.Fatalf("session runtime class ignored: %v", pod.Spec.RuntimeClassName)
	}
	s.RuntimeClass = ""
	cfg.RuntimeClass = ""
	if pod := BuildPod(s, cfg); pod.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class set without one: %v", *pod.Spec.RuntimeClassName)
	}
}

func TestClampExtendedResources(t *testing.T) {
	def := testCfg().DefaultResources
	ceiling := config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "2"}}}
	qty := func(l corev1.ResourceList, name string) string {
		q := l[corev1.ResourceName(name)]
		return q.String()
	}
	// Nothing asked and no default: nothing set, even with a maximum.
	rr := ClampResources(config.Resources{}, def, ceiling)
	if _, ok := rr.Limits["nvidia.com/gpu"]; ok {
		t.Fatalf("gpu handed out without a default: %+v", rr)
	}
	// Asked within the maximum: limit and request both set, equal.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "1"}}}, def, ceiling)
	if qty(rr.Limits, "nvidia.com/gpu") != "1" || qty(rr.Requests, "nvidia.com/gpu") != "1" {
		t.Fatalf("gpu: %+v", rr)
	}
	// Above the maximum: clamped. Unknown to the chart: passed through.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "8", "hugepages-2Mi": "64Mi"}}}, def, ceiling)
	if qty(rr.Limits, "nvidia.com/gpu") != "2" || qty(rr.Limits, "hugepages-2Mi") != "64Mi" {
		t.Fatalf("clamp: %+v", rr)
	}
	// A default extended resource goes to every session and, without a
	// maximum of its own, is also the cap.
	def.Limits.Extended = map[string]string{"nvidia.com/gpu": "1"}
	rr = ClampResources(config.Resources{}, def, config.Resources{})
	if qty(rr.Limits, "nvidia.com/gpu") != "1" {
		t.Fatalf("default gpu: %+v", rr)
	}
	rr = ClampResources(config.Resources{Limits: config.ResourceList{Extended: map[string]string{"nvidia.com/gpu": "4"}}}, def, config.Resources{})
	if qty(rr.Limits, "nvidia.com/gpu") != "1" {
		t.Fatalf("default as cap: %+v", rr)
	}
	// CPU and memory are untouched by all this.
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" {
		t.Fatalf("cpu/mem: %+v", rr)
	}
}

func TestClampMaxResources(t *testing.T) {
	def := testCfg().DefaultResources
	ceiling := config.Resources{Limits: config.ResourceList{CPU: "8", Memory: "32Gi"}}
	// Raising above the default is fine up to the maximum.
	rr := ClampResources(config.Resources{Limits: config.ResourceList{CPU: "6", Memory: "16Gi"}}, def, ceiling)
	if rr.Limits.Cpu().String() != "6" || rr.Limits.Memory().String() != "16Gi" {
		t.Fatalf("raise: %+v", rr)
	}
	// Beyond it: clamped; requests follow the clamped limit.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "64", Memory: "1Ti"}, Requests: config.ResourceList{CPU: "32"}}, def, ceiling)
	if rr.Limits.Cpu().String() != "8" || rr.Limits.Memory().String() != "32Gi" || rr.Requests.Cpu().String() != "8" {
		t.Fatalf("clamp: %+v", rr)
	}
	// Nothing asked: still the defaults, not the maximum.
	rr = ClampResources(config.Resources{}, def, ceiling)
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" {
		t.Fatalf("defaults: %+v", rr)
	}
	// A maximum that only names cpu leaves memory capped at its default.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "6", Memory: "16Gi"}}, def, config.Resources{Limits: config.ResourceList{CPU: "8"}})
	if rr.Limits.Cpu().String() != "6" || rr.Limits.Memory().String() != "4Gi" {
		t.Fatalf("partial max: %+v", rr)
	}
}

func TestBuildPodK8sAccess(t *testing.T) {
	s := testSession()
	cfg := testCfg()
	cfg.ServiceAccount = "runner-view"
	envOf := func(pod *corev1.Pod, name string) string {
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Name == name {
				return e.Value
			}
		}
		return "<unset>"
	}
	// Off: no identity, as before.
	pod := BuildPod(s, cfg)
	if pod.Spec.ServiceAccountName != "" || *pod.Spec.AutomountServiceAccountToken || envOf(pod, runner.EnvK8sAccess) != "off" {
		t.Fatalf("identity without asking: %+v", pod.Spec)
	}
	// Read-only: the shared account with its token mounted.
	s.K8sAccess = store.K8sAccessReadOnly
	pod = BuildPod(s, cfg)
	if pod.Spec.ServiceAccountName != "runner-view" || !*pod.Spec.AutomountServiceAccountToken || envOf(pod, runner.EnvK8sAccess) != "readonly" {
		t.Fatalf("read-only not applied: %+v", pod.Spec)
	}
	// Read-only asked but the chart offers none: nothing.
	cfg.ServiceAccount = ""
	pod = BuildPod(s, cfg)
	if pod.Spec.ServiceAccountName != "" || *pod.Spec.AutomountServiceAccountToken || envOf(pod, runner.EnvK8sAccess) != "off" {
		t.Fatalf("identity without a configured account: %+v", pod.Spec)
	}
	// Namespace mode: the session's own account, and the runner is told where it may write.
	cfg.ServiceAccount, cfg.NamespaceWrite = "runner-view", true
	s.K8sAccess, s.K8sNamespaces = store.K8sAccessNamespace, []string{"dev", "staging"}
	pod = BuildPod(s, cfg)
	if pod.Spec.ServiceAccountName != SessionAccountName(s.ID) || !*pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("namespace mode not applied: %+v", pod.Spec)
	}
	if envOf(pod, runner.EnvK8sAccess) != "namespace" || envOf(pod, runner.EnvK8sNamespaces) != "dev,staging" {
		t.Fatalf("runner env: %s %s", envOf(pod, runner.EnvK8sAccess), envOf(pod, runner.EnvK8sNamespaces))
	}
	// Namespace mode on a hub without it: no identity rather than the wrong one.
	cfg.NamespaceWrite = false
	pod = BuildPod(s, cfg)
	if pod.Spec.ServiceAccountName != "" || *pod.Spec.AutomountServiceAccountToken || envOf(pod, runner.EnvK8sAccess) != "off" {
		t.Fatalf("namespace mode without namespaceWrite: %+v", pod.Spec)
	}
}

func TestPersonaAndAgentEnv(t *testing.T) {
	s := testSession()
	s.Agent = runner.AgentOpenCode
	envOf := func() map[string]string {
		m := map[string]string{}
		for _, e := range BuildPod(s, testCfg()).Spec.Containers[0].Env {
			m[e.Name] = e.Value
		}
		return m
	}
	if env := envOf(); env[runner.EnvPersona] != "" || env[runner.EnvAgentDefinition] != "" || env[runner.EnvHandle] != "" {
		t.Fatalf("env without either: %v", env)
	}
	s.Persona = "Be terse."
	s.Handle = "eager_turing"
	s.AgentDefinition = "---\nname: red-team\ndescription: Reviewer\n---\nYou review code."
	env := envOf()
	if env[runner.EnvPersona] != "Be terse." || env[runner.EnvHandle] != "eager_turing" ||
		env[runner.EnvAgentDefinition] != s.AgentDefinition || env[runner.EnvAgentDefinitionName] != "red-team" {
		t.Fatalf("env = %v", env)
	}
	// A definition that no longer parses is left out.
	s.AgentDefinition = "not a definition"
	if env := envOf(); env[runner.EnvAgentDefinition] != "" || env[runner.EnvAgentDefinitionName] != "" {
		t.Fatalf("bad definition passed: %v", env)
	}
}
