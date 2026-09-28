package reconcile

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

func testCfg() Config {
	return Config{
		Namespace: "agenthub", RunnerImage: "ghcr.io/x/agenthub-runner", RunnerImageTag: "1.0.0",
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
		RepoURL: "git@github.com:x/y.git", Branch: "main", Autonomous: true, Generation: 3,
		Env:          map[string]string{"FOO": "bar"},
		NodeSelector: map[string]string{"kubernetes.io/arch": "arm64"},
	}
}

func TestBuildPVC(t *testing.T) {
	s := testSession()
	pvc := BuildPVC(s, testCfg())
	if pvc.Name != "agenthub-"+s.ID || *pvc.Spec.StorageClassName != "nfs-fast" {
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

func TestBuildSecret(t *testing.T) {
	s := testSession()
	sec := BuildSecret(s, testCfg(), "tok", map[string][]byte{
		store.CredAnthropicAPIKey: []byte("sk-ant"),
		store.CredGitSSHKey:       []byte("-----BEGIN"),
		store.CredClaudeLogin:     []byte(`{"a":1}`),
		"unknown":                 []byte("x"),
	})
	if string(sec.Data["RUNNER_TOKEN"]) != "tok" || string(sec.Data["ANTHROPIC_API_KEY"]) != "sk-ant" || string(sec.Data["GIT_SSH_KEY"]) != "-----BEGIN" {
		t.Fatalf("data = %v", sec.Data)
	}
	if string(sec.Data["AGENTHUB_LOGIN_CLAUDE_LOGIN"]) != "eyJhIjoxfQ==" {
		t.Fatalf("login seed = %q", sec.Data["AGENTHUB_LOGIN_CLAUDE_LOGIN"])
	}
	if _, ok := sec.Data["unknown"]; ok {
		t.Fatal("unknown key projected")
	}
	if sec.Annotations[k8s.AnnotationGeneration] != "3" {
		t.Fatalf("generation annotation = %v", sec.Annotations)
	}
	// No user secret at all still yields a token.
	sec = BuildSecret(s, testCfg(), "tok2", nil)
	if len(sec.Data) != 1 || string(sec.Data["RUNNER_TOKEN"]) != "tok2" {
		t.Fatalf("data = %v", sec.Data)
	}
}

func TestClampResources(t *testing.T) {
	def := testCfg().DefaultResources
	// No overrides: defaults.
	rr := ClampResources(config.Resources{}, def)
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" || rr.Requests.Cpu().String() != "250m" {
		t.Fatalf("defaults: %+v", rr)
	}
	// Lowering is allowed.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "500m", Memory: "1Gi"}}, def)
	if rr.Limits.Cpu().String() != "500m" || rr.Limits.Memory().String() != "1Gi" {
		t.Fatalf("lower: %+v", rr)
	}
	// Raising is clamped to the default, and requests never exceed limits.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{CPU: "8", Memory: "64Gi"}, Requests: config.ResourceList{CPU: "4"}}, def)
	if rr.Limits.Cpu().String() != "2" || rr.Limits.Memory().String() != "4Gi" || rr.Requests.Cpu().String() != "2" {
		t.Fatalf("raise: %+v", rr)
	}
	// Request above a lowered limit is pulled down.
	rr = ClampResources(config.Resources{Limits: config.ResourceList{Memory: "256Mi"}}, def)
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
	if pod.Name != ObjectName(s.ID) || pod.Namespace != "agenthub" || pod.Spec.Hostname != "my-session" {
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
	if c.Image != "ghcr.io/x/agenthub-runner:1.0.0" {
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
	if env["AGENT"] != "claude" || env["AUTONOMOUS"] != "true" || env["REPO_URL"] != s.RepoURL || env["REPO_BRANCH"] != "main" || env["FOO"] != "bar" || env["ANTHROPIC_BASE_URL"] != "http://cliproxy" {
		t.Fatalf("env = %v", env)
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
	if BuildPod(s, testCfg()).Spec.Containers[0].Image != "ghcr.io/x/agenthub-runner:dev" {
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
