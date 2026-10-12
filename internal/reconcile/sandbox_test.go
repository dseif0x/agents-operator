package reconcile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

func sandboxCfg() Config {
	cfg := testCfg()
	cfg.HubServiceAccount = "agents-operator"
	cfg.NetworkPolicy = true
	cfg.Sandbox = config.Sandbox{
		Enabled: true, Domain: "sandbox.example.com", NamespacePrefix: "sandbox-", PodSecurity: "baseline",
		ChartRepo: "https://charts.loft.sh", ChartName: "vcluster", ChartVersion: "0.37.3", InstallerImage: "alpine/helm:3.22.0",
		HubRole: "ao-sandbox-hub", InstallerRole: "ao-sandbox-installer",
		IngressFrom: json.RawMessage(`[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"traefik"}}}]`),
	}
	return cfg
}

func sandboxSession() *store.Session {
	s := testSession()
	s.Name = "Cluster A"
	s.K8sAccess = store.K8sAccessSandbox
	s.Egress = nil
	return s
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func TestSandboxValues(t *testing.T) {
	cfg := sandboxCfg()
	// The chart's extra values merge in, but cannot undo the isolation.
	cfg.Sandbox.Values = map[string]any{
		"rbac":         map[string]any{"clusterRole": map[string]any{"enabled": true}},
		"controlPlane": map[string]any{"statefulSet": map[string]any{"scheduling": map[string]any{"nodeSelector": map[string]any{"kubernetes.io/arch": "amd64"}}}},
		"policies":     map[string]any{"resourceQuota": map[string]any{"quota": map[string]any{"requests.cpu": "4", "services.loadbalancers": 3}}},
	}
	s := sandboxSession()
	v := SandboxValues(s, cfg)
	server := "vcluster.sandbox-" + s.ID + ".svc"
	checks := []struct {
		path []string
		want any
	}{
		{[]string{"rbac", "clusterRole", "enabled"}, false},
		{[]string{"exportKubeConfig", "server"}, "https://" + server + ":443"},
		{[]string{"sync", "toHost", "ingresses", "enabled"}, true},
		{[]string{"telemetry", "enabled"}, false},
		{[]string{"policies", "resourceQuota", "quota", "services.loadbalancers"}, 0},
		{[]string{"policies", "resourceQuota", "quota", "services.nodeports"}, 0},
		{[]string{"policies", "resourceQuota", "quota", "requests.cpu"}, "4"},
		{[]string{"policies", "networkPolicy", "enabled"}, true},
		{[]string{"controlPlane", "statefulSet", "scheduling", "nodeSelector", "kubernetes.io/arch"}, "amd64"},
	}
	for _, c := range checks {
		if got := dig(v, c.path...); got != c.want {
			t.Errorf("%s = %v, want %v", strings.Join(c.path, "."), got, c.want)
		}
	}
	if sans, _ := dig(v, "controlPlane", "proxy", "extraSANs").([]any); len(sans) != 1 || sans[0] != server {
		t.Errorf("extraSANs = %v", sans)
	}
	// The chart's values map is not modified by merging.
	if cfg.Sandbox.Values["rbac"].(map[string]any)["clusterRole"].(map[string]any)["enabled"] != true {
		t.Error("input values were mutated")
	}
	// No domain: no Ingress sync.
	cfg.Sandbox.Domain = ""
	if dig(SandboxValues(s, cfg), "sync", "toHost", "ingresses", "enabled") != false {
		t.Error("ingress sync without a domain")
	}
}

func TestSandboxInstallHash(t *testing.T) {
	cfg := sandboxCfg()
	s := sandboxSession()
	a, _ := newSandboxInstall(s, cfg)
	b, _ := newSandboxInstall(s, cfg)
	if a.hash != b.hash || len(a.hash) != 16 {
		t.Fatalf("hash not stable: %s %s", a.hash, b.hash)
	}
	cfg.Sandbox.ChartVersion = "0.38.0"
	c, _ := newSandboxInstall(s, cfg)
	cfg = sandboxCfg()
	cfg.Sandbox.Domain = ""
	d, _ := newSandboxInstall(s, cfg)
	if c.hash == a.hash || d.hash == a.hash {
		t.Fatal("hash ignores chart version or Ingress sync")
	}
	// Another domain only moves the host annotation: no reinstall.
	cfg.Sandbox.Domain = "other.example.com"
	if e, _ := newSandboxInstall(s, cfg); e.hash != a.hash {
		t.Fatal("a domain change reinstalls")
	}
}

func TestBuildSandboxNamespaceAndInstaller(t *testing.T) {
	cfg := sandboxCfg()
	s := sandboxSession()
	ns := BuildSandboxNamespace(s, cfg)
	if ns.Name != "sandbox-"+s.ID || ns.Labels[k8s.LabelSandbox] != "true" || ns.Labels[k8s.LabelSession] != s.ID ||
		ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" || ns.Annotations[k8s.AnnotationSandboxHost] != "cluster-a.sandbox.example.com" {
		t.Fatalf("namespace = %+v", ns.ObjectMeta)
	}
	hub := BuildSandboxHubBinding(s, cfg)
	if hub.Namespace != ns.Name || hub.RoleRef.Name != "ao-sandbox-hub" || hub.Subjects[0].Name != "agents-operator" || hub.Subjects[0].Namespace != "agents-operator" {
		t.Fatalf("hub binding = %+v", hub)
	}
	pol, err := BuildSandboxIngressPolicy(s, cfg)
	if err != nil || pol == nil || pol.Spec.PodSelector.MatchLabels["vcluster.loft.sh/managed-by"] != "vcluster" || len(pol.Spec.Ingress[0].From) != 1 {
		t.Fatalf("ingress policy = %+v, %v", pol, err)
	}

	in, _ := newSandboxInstall(s, cfg)
	sa, rb, cm, job := BuildSandboxInstaller(s, cfg, in)
	if sa.Namespace != ns.Name || rb.RoleRef.Name != "ao-sandbox-installer" || rb.Subjects[0].Name != sa.Name || cm.Data["values.json"] != string(in.values) {
		t.Fatalf("installer objects: sa=%v rb=%v", sa.Name, rb.RoleRef)
	}
	c := job.Spec.Template.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	if c.Image != "alpine/helm:3.22.0" || !strings.Contains(args, "upgrade --install vcluster vcluster --repo https://charts.loft.sh --version 0.37.3 --namespace "+ns.Name) {
		t.Fatalf("installer = %s %s", c.Image, args)
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem || *job.Spec.Template.Spec.SecurityContext.RunAsUser != RunnerUID || job.Spec.Template.Spec.ServiceAccountName != sa.Name {
		t.Fatal("installer pod not hardened")
	}
	cfg.Sandbox.ChartName = "oci://ghcr.io/loft-sh/charts/vcluster"
	_, _, _, job = BuildSandboxInstaller(s, cfg, in)
	if slices.Contains(job.Spec.Template.Spec.Containers[0].Args, "--repo") {
		t.Fatal("--repo with an oci:// chart")
	}
}

func TestBuildPodSandbox(t *testing.T) {
	cfg := sandboxCfg()
	s := sandboxSession()
	s.Env = map[string]string{"KUBECONFIG": "/elsewhere", "FOO": "bar"}
	p := BuildPod(s, cfg)
	if *p.Spec.AutomountServiceAccountToken || p.Spec.ServiceAccountName != "" {
		t.Fatal("sandbox pod carries a host identity")
	}
	env := map[string]string{}
	count := 0
	for _, e := range p.Spec.Containers[0].Env {
		env[e.Name] = e.Value
		if e.Name == "KUBECONFIG" {
			count++
		}
	}
	if count != 1 || env["KUBECONFIG"] != SandboxKubeconfigDir+"/config" || env[runner.EnvK8sAccess] != store.K8sAccessSandbox ||
		env[runner.EnvSandboxNamespace] != "sandbox-"+s.ID || env[runner.EnvSandboxHost] != "cluster-a.sandbox.example.com" ||
		env[runner.EnvSandboxSecurity] != "baseline" || env["FOO"] != "bar" {
		t.Fatalf("env = %v (KUBECONFIG %d times)", env, count)
	}
	var mounted bool
	for _, m := range p.Spec.Containers[0].VolumeMounts {
		mounted = mounted || (m.MountPath == SandboxKubeconfigDir && m.ReadOnly)
	}
	var vol bool
	for _, v := range p.Spec.Volumes {
		vol = vol || (v.Secret != nil && v.Secret.SecretName == SandboxKubeconfigName(s.ID))
	}
	if !mounted || !vol {
		t.Fatal("kubeconfig not mounted")
	}
	// A hub without the mode builds a pod without any access.
	cfg.Sandbox.Enabled = false
	p = BuildPod(s, cfg)
	for _, e := range p.Spec.Containers[0].Env {
		if e.Name == runner.EnvK8sAccess && e.Value != store.K8sAccessOff {
			t.Fatalf("access = %s on a hub without sandbox mode", e.Value)
		}
	}
}

func TestBuildEgressPolicySandbox(t *testing.T) {
	cfg := sandboxCfg()
	s := sandboxSession()
	pol, err := BuildEgressPolicy(s, cfg)
	if err != nil || pol == nil || len(pol.Spec.Egress) != 1 {
		t.Fatalf("policy = %+v, %v", pol, err)
	}
	rule := pol.Spec.Egress[0]
	if rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "sandbox-"+s.ID ||
		rule.To[0].PodSelector.MatchLabels["app"] != "vcluster" || rule.Ports[0].Port.IntValue() != 8443 {
		t.Fatalf("sandbox rule = %+v", rule)
	}
	s.Egress = []string{"192.168.1.10"}
	if pol, _ = BuildEgressPolicy(s, cfg); len(pol.Spec.Egress) != 2 {
		t.Fatalf("rules = %d, want sandbox plus one destination", len(pol.Spec.Egress))
	}
}

// sandboxHarness is a reconciler with the sandbox mode on.
func sandboxHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.r.cfg = sandboxCfg()
	return h
}

func (h *harness) sandboxNS(id string) *corev1.Namespace {
	ns, err := h.cs.CoreV1().Namespaces().Get(h.ctx, "sandbox-"+id, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return ns
}

func (h *harness) installerJob(id string) *batchv1.Job {
	h.t.Helper()
	list, err := h.cs.BatchV1().Jobs("sandbox-"+id).List(h.ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return nil
	}
	return &list.Items[0]
}

func (h *harness) finishJob(j *batchv1.Job, t batchv1.JobConditionType) {
	h.t.Helper()
	j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "helm failed"})
	if _, err := h.cs.BatchV1().Jobs(j.Namespace).UpdateStatus(h.ctx, j, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

// installSandbox drives a new sandbox session up to the installer Job.
func (h *harness) installSandbox(s *store.Session) *batchv1.Job {
	h.t.Helper()
	h.reconcile(s.ID) // pvc + secret
	h.eventually("pvc and secret in cache", func() bool {
		o := h.r.observe(s.ID)
		return o.pvc != nil && o.secret != nil
	})
	h.reconcile(s.ID) // namespace
	if h.sandboxNS(s.ID) == nil {
		h.t.Fatal("no sandbox namespace")
	}
	h.reconcile(s.ID) // hub binding, ingress policy, installer
	job := h.installerJob(s.ID)
	if job == nil {
		h.t.Fatalf("no installer job; state %s", h.session(s.ID).StateReason)
	}
	return job
}

func TestSandboxLifecycle(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	s.State = store.StateCreating
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	job := h.installSandbox(s)
	ns := "sandbox-" + s.ID
	if got := h.session(s.ID); got.State != store.StateCreating || !strings.Contains(got.StateReason, "installing vcluster") {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	if h.pod(s.ID) != nil {
		t.Fatal("pod created before the sandbox is ready")
	}
	if rb, err := h.cs.RbacV1().RoleBindings(ns).Get(h.ctx, sandboxHubBinding, metav1.GetOptions{}); err != nil || rb.RoleRef.Name != "ao-sandbox-hub" {
		t.Fatalf("hub binding: %v", err)
	}
	if _, err := h.cs.NetworkingV1().NetworkPolicies(ns).Get(h.ctx, sandboxIngressPolicy, metav1.GetOptions{}); err != nil {
		t.Fatalf("ingress policy: %v", err)
	}
	if _, err := h.cs.RbacV1().RoleBindings(ns).Get(h.ctx, sandboxInstaller, metav1.GetOptions{}); err != nil {
		t.Fatalf("installer binding: %v", err)
	}
	// Another pass while it runs creates nothing new.
	h.reconcile(s.ID)
	if list, _ := h.cs.BatchV1().Jobs(ns).List(h.ctx, metav1.ListOptions{}); len(list.Items) != 1 {
		t.Fatalf("jobs = %d", len(list.Items))
	}

	// The install succeeds: recorded on the namespace, installer rights gone.
	h.finishJob(job, batchv1.JobComplete)
	h.reconcile(s.ID)
	in, _ := newSandboxInstall(s, h.r.cfg)
	if h.sandboxNS(s.ID).Annotations[k8s.AnnotationSandboxConfig] != in.hash {
		t.Fatal("install not recorded")
	}
	if _, err := h.cs.RbacV1().RoleBindings(ns).Get(h.ctx, sandboxInstaller, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("installer binding kept after the install")
	}
	if _, err := h.cs.CoreV1().ServiceAccounts(ns).Get(h.ctx, sandboxInstaller, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("installer account kept after the install")
	}
	// vcluster writes its kubeconfig; the next pass copies it and creates the pod.
	if _, err := h.cs.CoreV1().Secrets(ns).Create(h.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vc-vcluster", Namespace: ns},
		Data:       map[string][]byte{"config": []byte("apiVersion: v1\nkind: Config\n")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	kc, err := h.cs.CoreV1().Secrets(h.ns).Get(h.ctx, SandboxKubeconfigName(s.ID), metav1.GetOptions{})
	if err != nil || string(kc.Data["config"]) != "apiVersion: v1\nkind: Config\n" || kc.Labels[k8s.LabelSession] != s.ID {
		t.Fatalf("kubeconfig secret: %v", err)
	}
	h.eventually("pod in cache", func() bool { return h.r.observe(s.ID).pod != nil })
	if _, err := h.cs.NetworkingV1().NetworkPolicies(h.ns).Get(h.ctx, ObjectName(s.ID), metav1.GetOptions{}); err != nil {
		t.Fatalf("session egress policy to the vcluster API: %v", err)
	}
	h.markReady(s.ID)
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateRunning {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}

	// Stop: the sandbox keeps running.
	if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateStopping, ""); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("pod deleted", func() bool { return h.r.observe(s.ID).pod == nil })
	h.reconcile(s.ID)
	if h.session(s.ID).State != store.StateStopped || h.sandboxNS(s.ID) == nil {
		t.Fatal("stop removed the sandbox")
	}

	// Delete: namespace and kubeconfig go.
	if _, err := h.st.Sessions().SetState(h.ctx, s.ID, store.StateDeleting, ""); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("objects gone", func() bool {
		o := h.r.observe(s.ID)
		return o.pod == nil && o.pvc == nil && o.secret == nil
	})
	h.reconcile(s.ID)
	if h.session(s.ID) != nil || h.sandboxNS(s.ID) != nil {
		t.Fatal("delete left the sandbox")
	}
	if _, err := h.cs.CoreV1().Secrets(h.ns).Get(h.ctx, SandboxKubeconfigName(s.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("delete left the kubeconfig")
	}
}

func TestSandboxInstallFailureFailsSession(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	s.State = store.StateCreating
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	job := h.installSandbox(s)
	h.finishJob(job, batchv1.JobFailed)
	h.reconcile(s.ID)
	got := h.session(s.ID)
	if got.State != store.StateFailed || !strings.Contains(got.StateReason, "installing vcluster failed") || !strings.Contains(got.StateReason, job.Name) {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
}

func TestSandboxConfigChangeReinstalls(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	s.State = store.StateCreating
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	first := h.installSandbox(s)
	h.r.cfg.Sandbox.ChartVersion = "0.38.0"
	h.reconcile(s.ID)
	list, _ := h.cs.BatchV1().Jobs("sandbox-"+s.ID).List(h.ctx, metav1.ListOptions{})
	if len(list.Items) != 1 || list.Items[0].Name == first.Name || !slices.Contains(list.Items[0].Spec.Template.Spec.Containers[0].Args, "0.38.0") {
		t.Fatalf("jobs after a version change = %d", len(list.Items))
	}
}

func TestSandboxSwitchedAwayIsRemoved(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	s.State = store.StateCreating
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	h.installSandbox(s)
	edited := *h.session(s.ID)
	edited.K8sAccess = store.K8sAccessOff
	if _, err := h.st.Sessions().Update(h.ctx, &edited); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	if h.sandboxNS(s.ID) != nil {
		t.Fatal("sandbox kept after switching the mode off")
	}
	h.eventually("pod in cache", func() bool { return h.r.observe(s.ID).pod != nil })
}

func TestSandboxForeignNamespaceFails(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	s.State = store.StateCreating
	if err := h.st.Sessions().Create(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().Namespaces().Create(h.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-" + s.ID}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.reconcile(s.ID)
	h.eventually("pvc and secret in cache", func() bool {
		o := h.r.observe(s.ID)
		return o.pvc != nil && o.secret != nil
	})
	h.reconcile(s.ID)
	if got := h.session(s.ID); got.State != store.StateFailed || !strings.Contains(got.StateReason, "not this session's sandbox") {
		t.Fatalf("state = %s (%s)", got.State, got.StateReason)
	}
	// It is never deleted, by a session delete or as an orphan.
	if err := h.r.removeSandbox(h.ctx, nil, s.ID); err != nil || h.sandboxNS(s.ID) == nil {
		t.Fatalf("foreign namespace touched: %v", err)
	}
}

func TestOrphanedSandboxIsRemoved(t *testing.T) {
	h := sandboxHarness(t)
	s := sandboxSession()
	ns := BuildSandboxNamespace(s, h.r.cfg)
	ns.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	if _, err := h.cs.CoreV1().Namespaces().Create(h.ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ids := h.r.sandboxSessionIDs(h.ctx); !slices.Contains(ids, s.ID) {
		t.Fatalf("orphan not listed: %v", ids)
	}
	h.reconcile(s.ID)
	if h.sandboxNS(s.ID) != nil {
		t.Fatal("orphaned sandbox kept")
	}
}
