package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Kubernetes access in "sandbox" mode: the session gets a virtual cluster
// (vcluster) of its own and only that cluster's kubeconfig. The agent is
// cluster-admin inside it; on the host everything it creates lands as plain
// objects in one namespace made for the session.
//
// The hub creates that namespace (labelled, with the chart's Pod Security
// level enforced), binds itself the chart's sandbox-hub role there, opens
// the sandbox's workloads to the ingress controller, and installs vcluster
// with a one-off Job running the Helm CLI under a ServiceAccount bound to the
// chart's installer role. Once the Job succeeds the installer's binding and
// account are removed, the namespace remembers the installed configuration
// by hash, and the hub copies the vcluster's kubeconfig (secret vc-vcluster,
// pointed at the in-cluster service) into a Secret in its own namespace that
// the session pod mounts. A configuration change (chart, installer image or
// values, including turning the sandbox domain on or off) installs again
// on the next start; the Ingress host follows a renamed session or another
// domain without one, through the namespace annotation.
//
// The sandbox outlives a stopped session, so whatever runs in it stays up
// (a demo, a long test); it is deleted with the session, or on the next
// start after the session switched to another mode. The chart's admission
// policies keep the hub's namespace rights to sandbox namespaces and the
// sandbox's Ingress hosts below its own DNS name; vcluster's ResourceQuota
// allows no LoadBalancer or NodePort Services and its NetworkPolicies keep
// the workloads off private ranges.

const (
	// SandboxRelease is the Helm release (and vcluster) name in every
	// sandbox namespace; the service and kubeconfig secret derive from it.
	SandboxRelease = "vcluster"
	// sandboxKubeconfigSecret is where vcluster stores its kubeconfig.
	sandboxKubeconfigSecret = "vc-" + SandboxRelease
	// sandboxHubBinding binds the hub its sandbox role in the namespace.
	sandboxHubBinding = "agents-operator-hub"
	// sandboxInstaller names the installer's ServiceAccount, RoleBinding
	// and values ConfigMap.
	sandboxInstaller = "agents-operator-sandbox-installer"
	// sandboxIngressPolicy opens the workloads to the ingress controller.
	sandboxIngressPolicy = "agents-operator-sandbox-ingress"
	// SandboxKubeconfigDir is where the session pod mounts the kubeconfig.
	SandboxKubeconfigDir = "/var/run/agents-operator/sandbox"
	// labelInstaller marks installer Jobs.
	labelInstaller = "agents-operator.io/sandbox-installer"
)

// SandboxNamespace is the host namespace of a session's sandbox.
func SandboxNamespace(sessionID string, cfg Config) string {
	return cfg.Sandbox.NamespacePrefix + sessionID
}

// SandboxHost is the DNS name the sandbox's Ingress hosts must equal or end
// in, <session name>.<domain>, or "" when the hub has no sandbox domain.
func SandboxHost(s *store.Session, cfg Config) string {
	if cfg.Sandbox.Domain == "" {
		return ""
	}
	return Hostname(s.Name) + "." + cfg.Sandbox.Domain
}

// SandboxKubeconfigName is the hub-namespace Secret holding the kubeconfig.
func SandboxKubeconfigName(sessionID string) string { return ObjectName(sessionID) + "-kubeconfig" }

// sandboxServer is the vcluster API address seen from inside the cluster.
func sandboxServer(sessionID string, cfg Config) string {
	return SandboxRelease + "." + SandboxNamespace(sessionID, cfg) + ".svc"
}

// sandboxLabels are the labels of a sandbox namespace and what the hub
// creates in it.
func sandboxLabels(s *store.Session) map[string]string {
	l := Labels(s)
	l[k8s.LabelSandbox] = "true"
	l[k8s.LabelComponent] = "sandbox"
	return l
}

// SandboxValues returns the vcluster chart values for a session: the hub's
// defaults, the chart's extra values over them, and last the settings the
// sandbox's isolation depends on, which nothing overrides.
func SandboxValues(s *store.Session, cfg Config) map[string]any {
	server := sandboxServer(s.ID, cfg)
	values := map[string]any{
		"telemetry": map[string]any{"enabled": false},
		"sync": map[string]any{
			"toHost": map[string]any{"ingresses": map[string]any{"enabled": cfg.Sandbox.Domain != ""}},
			// Host classes need a ClusterRole, which a sandbox never gets.
			"fromHost": map[string]any{
				"storageClasses":        map[string]any{"enabled": false},
				"volumeSnapshotClasses": map[string]any{"enabled": false},
			},
		},
		"policies": map[string]any{
			"resourceQuota": map[string]any{"enabled": true},
			"limitRange":    map[string]any{"enabled": true},
			"networkPolicy": map[string]any{"enabled": true},
		},
	}
	mergeValues(values, cfg.Sandbox.Values)
	mergeValues(values, map[string]any{
		"rbac":             map[string]any{"clusterRole": map[string]any{"enabled": false}},
		"exportKubeConfig": map[string]any{"server": "https://" + server + ":443"},
		"controlPlane":     map[string]any{"proxy": map[string]any{"extraSANs": []any{server}}},
		"policies": map[string]any{"resourceQuota": map[string]any{"quota": map[string]any{
			"services.loadbalancers": 0, "services.nodeports": 0,
		}}},
	})
	return values
}

// mergeValues deep-merges src into dst; maps merge, everything else replaces.
func mergeValues(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeValues(dv, sv)
				continue
			}
			cp := map[string]any{}
			mergeValues(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// sandboxInstall is what one install of a sandbox consists of: the values
// file and a hash over everything that shapes the result.
type sandboxInstall struct {
	values []byte
	hash   string
}

func newSandboxInstall(s *store.Session, cfg Config) (sandboxInstall, error) {
	values, err := json.Marshal(SandboxValues(s, cfg))
	if err != nil {
		return sandboxInstall{}, err
	}
	h := sha256.New()
	for _, part := range []string{cfg.Sandbox.ChartRepo, cfg.Sandbox.ChartName, cfg.Sandbox.ChartVersion, cfg.Sandbox.InstallerImage, string(values)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return sandboxInstall{values: values, hash: hex.EncodeToString(h.Sum(nil))[:16]}, nil
}

// BuildSandboxNamespace returns the sandbox's namespace.
func BuildSandboxNamespace(s *store.Session, cfg Config) *corev1.Namespace {
	l := sandboxLabels(s)
	l["pod-security.kubernetes.io/enforce"] = cfg.Sandbox.PodSecurity
	l["pod-security.kubernetes.io/enforce-version"] = "latest"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: SandboxNamespace(s.ID, cfg), Labels: l, Annotations: map[string]string{}}}
	if host := SandboxHost(s, cfg); host != "" {
		ns.Annotations[k8s.AnnotationSandboxHost] = host
	}
	return ns
}

// BuildSandboxHubBinding binds the hub the chart's sandbox-hub role in the
// sandbox namespace.
func BuildSandboxHubBinding(s *store.Session, cfg Config) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxHubBinding, Namespace: SandboxNamespace(s.ID, cfg), Labels: sandboxLabels(s)},
		RoleRef:    clusterRoleRef(cfg.Sandbox.HubRole),
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: cfg.HubServiceAccount, Namespace: cfg.Namespace}},
	}
}

// BuildSandboxIngressPolicy lets the configured peers (the ingress
// controller) reach the sandbox's workloads, which vcluster's own policies
// close to everything outside the vcluster. Nil without peers or domain.
func BuildSandboxIngressPolicy(s *store.Session, cfg Config) (*networkingv1.NetworkPolicy, error) {
	if cfg.Sandbox.Domain == "" || len(cfg.Sandbox.IngressFrom) == 0 {
		return nil, nil
	}
	var peers []networkingv1.NetworkPolicyPeer
	if err := json.Unmarshal(cfg.Sandbox.IngressFrom, &peers); err != nil {
		return nil, fmt.Errorf("sandbox ingressFrom: %w", err)
	}
	if len(peers) == 0 {
		return nil, nil
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxIngressPolicy, Namespace: SandboxNamespace(s.ID, cfg), Labels: sandboxLabels(s)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"vcluster.loft.sh/managed-by": SandboxRelease}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: peers}},
		},
	}, nil
}

// SandboxJobName is the installer Job for one configuration hash.
func SandboxJobName(hash string) string { return "agents-operator-sandbox-install-" + hash[:10] }

// BuildSandboxInstaller returns the installer's ServiceAccount, its
// RoleBinding to the chart's installer role, the values ConfigMap and the
// Job running `helm upgrade --install`.
func BuildSandboxInstaller(s *store.Session, cfg Config, in sandboxInstall) (*corev1.ServiceAccount, *rbacv1.RoleBinding, *corev1.ConfigMap, *batchv1.Job) {
	ns := SandboxNamespace(s.ID, cfg)
	labels := sandboxLabels(s)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sandboxInstaller, Namespace: ns, Labels: labels}}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxInstaller, Namespace: ns, Labels: labels},
		RoleRef:    clusterRoleRef(cfg.Sandbox.InstallerRole),
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sandboxInstaller, Namespace: ns}},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxInstaller, Namespace: ns, Labels: labels},
		Data:       map[string]string{"values.json": string(in.values)},
	}
	args := []string{"upgrade", "--install", SandboxRelease, cfg.Sandbox.ChartName}
	if !strings.HasPrefix(cfg.Sandbox.ChartName, "oci://") {
		args = append(args, "--repo", cfg.Sandbox.ChartRepo)
	}
	args = append(args, "--version", cfg.Sandbox.ChartVersion, "--namespace", ns,
		"--values", "/values/values.json", "--wait", "--timeout", "10m")
	jobLabels := maps.Clone(labels)
	jobLabels[labelInstaller] = "true"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: SandboxJobName(in.hash), Namespace: ns, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](2),
			ActiveDeadlineSeconds:   ptr.To[int64](15 * 60),
			TTLSecondsAfterFinished: ptr.To[int32](24 * 3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: sandboxInstaller,
					RestartPolicy:      corev1.RestartPolicyNever,
					EnableServiceLinks: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(RunnerUID),
						RunAsGroup:     ptr.To(RunnerUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "helm",
						Image:   cfg.Sandbox.InstallerImage,
						Command: []string{"helm"},
						Args:    args,
						Env: []corev1.EnvVar{
							{Name: "HOME", Value: "/tmp"},
							{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
							{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
							{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "values", MountPath: "/values", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							RunAsNonRoot:             ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "values", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: sandboxInstaller}}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	return sa, rb, cm, job
}

// BuildSandboxKubeconfig returns the hub-namespace Secret the session pod
// mounts: the vcluster's kubeconfig as `config`.
func BuildSandboxKubeconfig(s *store.Session, cfg Config, kubeconfig []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SandboxKubeconfigName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"config": kubeconfig},
	}
}

// ensureSandbox converges a session's sandbox. It returns the vcluster's
// kubeconfig once the sandbox is installed, else a reason to wait with
// (and to show the owner). Owner-fixable failures are accessErrors.
func (r *Reconciler) ensureSandbox(ctx context.Context, sess *store.Session) ([]byte, string, error) {
	if !r.cfg.Sandbox.Enabled {
		return nil, "", &accessError{"sandbox clusters are not enabled on this hub (chart value runner.sandbox.enabled)"}
	}
	in, err := newSandboxInstall(sess, r.cfg)
	if err != nil {
		return nil, "", &accessError{"sandbox values: " + err.Error()}
	}
	want := BuildSandboxNamespace(sess, r.cfg)
	nsAPI := r.cs.CoreV1().Namespaces()
	have, err := nsAPI.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := nsAPI.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, "", wrapAccess("create sandbox namespace", err)
		}
		r.event(ctx, sess, "sandbox", "created namespace "+want.Name)
		return nil, "sandbox: namespace created", nil
	case err != nil:
		return nil, "", wrapAccess("read sandbox namespace", err)
	case have.DeletionTimestamp != nil:
		return nil, "sandbox: the previous sandbox namespace is still being deleted", nil
	case have.Labels[k8s.LabelSession] != sess.ID || have.Labels[k8s.LabelSandbox] != "true":
		return nil, "", &accessError{"namespace " + want.Name + " exists and is not this session's sandbox"}
	}
	// Labels (Pod Security level) and the Ingress host follow the chart and
	// the session name; the install hash is kept as it is.
	desired := have.DeepCopy()
	maps.Copy(desired.Labels, want.Labels)
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	if host := want.Annotations[k8s.AnnotationSandboxHost]; host != "" {
		desired.Annotations[k8s.AnnotationSandboxHost] = host
	} else {
		delete(desired.Annotations, k8s.AnnotationSandboxHost)
	}
	if !reflect.DeepEqual(desired.Labels, have.Labels) || !reflect.DeepEqual(desired.Annotations, have.Annotations) {
		if have, err = nsAPI.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return nil, "", wrapAccess("update sandbox namespace", err)
		}
	}
	ns := have.Name
	if err := r.ensureRoleBinding(ctx, BuildSandboxHubBinding(sess, r.cfg)); err != nil {
		return nil, "", err
	}
	if err := r.ensureSandboxIngressPolicy(ctx, sess); err != nil {
		return nil, "", err
	}

	if have.Annotations[k8s.AnnotationSandboxConfig] == in.hash {
		secret, err := r.cs.CoreV1().Secrets(ns).Get(ctx, sandboxKubeconfigSecret, metav1.GetOptions{})
		switch {
		case err == nil && len(secret.Data["config"]) > 0:
			return secret.Data["config"], "", nil
		case err != nil && !apierrors.IsNotFound(err):
			return nil, "", sandboxRetry("read vcluster kubeconfig", err)
		}
		// Installed but no kubeconfig yet (or gone): run the installer again.
	}
	wait, err := r.ensureSandboxInstaller(ctx, sess, in, have)
	return nil, wait, err
}

// ensureSandboxInstaller drives the installer Job for the wanted
// configuration and returns why the session still has to wait.
func (r *Reconciler) ensureSandboxInstaller(ctx context.Context, sess *store.Session, in sandboxInstall, nsObj *corev1.Namespace) (string, error) {
	ns := nsObj.Name
	sa, rb, cm, job := BuildSandboxInstaller(sess, r.cfg, in)
	jobs := r.cs.BatchV1().Jobs(ns)
	have, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", sandboxRetry("read installer job", err)
	default:
		switch {
		case jobCondition(have, batchv1.JobComplete):
			nsObj = nsObj.DeepCopy()
			if nsObj.Annotations == nil {
				nsObj.Annotations = map[string]string{}
			}
			nsObj.Annotations[k8s.AnnotationSandboxConfig] = in.hash
			if _, err := r.cs.CoreV1().Namespaces().Update(ctx, nsObj, metav1.UpdateOptions{}); err != nil {
				return "", sandboxRetry("record sandbox install", err)
			}
			// The installer's rights are only needed while it runs.
			if err := ignoreNotFound(r.cs.RbacV1().RoleBindings(ns).Delete(ctx, sandboxInstaller, metav1.DeleteOptions{})); err != nil {
				return "", sandboxRetry("remove installer binding", err)
			}
			if err := ignoreNotFound(r.cs.CoreV1().ServiceAccounts(ns).Delete(ctx, sandboxInstaller, metav1.DeleteOptions{})); err != nil {
				return "", sandboxRetry("remove installer account", err)
			}
			r.event(ctx, sess, "sandbox", "vcluster "+r.cfg.Sandbox.ChartVersion+" installed in "+ns)
			return "sandbox: vcluster installed, reading its kubeconfig", nil
		case jobCondition(have, batchv1.JobFailed):
			return "", &accessError{fmt.Sprintf("sandbox: installing vcluster failed (%s); see kubectl logs -n %s job/%s. Start the session again to retry",
				jobFailure(have), ns, have.Name)}
		default:
			return "sandbox: installing vcluster", nil
		}
	}

	// A new install: values, account, binding, then the Job. Installer
	// Jobs of other configurations go first.
	if err := r.removeInstallerJobs(ctx, ns, job.Name); err != nil {
		return "", err
	}
	if err := r.ensureConfigMap(ctx, cm); err != nil {
		return "", err
	}
	if _, err := r.cs.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", sandboxRetry("create installer account", err)
	}
	if err := r.ensureRoleBinding(ctx, rb); err != nil {
		return "", err
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", sandboxRetry("create installer job", err)
	}
	r.event(ctx, sess, "sandbox", "installing vcluster "+r.cfg.Sandbox.ChartVersion+" (job "+job.Name+")")
	return "sandbox: installing vcluster", nil
}

// removeInstallerJobs deletes installer Jobs other than keep.
func (r *Reconciler) removeInstallerJobs(ctx context.Context, ns, keep string) error {
	list, err := r.cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: labelInstaller})
	if err != nil {
		return sandboxRetry("list installer jobs", err)
	}
	for _, j := range list.Items {
		if j.Name == keep {
			continue
		}
		err := r.cs.BatchV1().Jobs(ns).Delete(ctx, j.Name, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)})
		if err := ignoreNotFound(err); err != nil {
			return sandboxRetry("delete old installer job", err)
		}
	}
	return nil
}

func (r *Reconciler) ensureConfigMap(ctx context.Context, want *corev1.ConfigMap) error {
	api := r.cs.CoreV1().ConfigMaps(want.Namespace)
	have, err := api.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := api.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return sandboxRetry("create installer values", err)
		}
	case err != nil:
		return sandboxRetry("read installer values", err)
	case !reflect.DeepEqual(have.Data, want.Data):
		have.Data = want.Data
		if _, err := api.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
			return sandboxRetry("update installer values", err)
		}
	}
	return nil
}

func (r *Reconciler) ensureSandboxIngressPolicy(ctx context.Context, sess *store.Session) error {
	want, err := BuildSandboxIngressPolicy(sess, r.cfg)
	if err != nil {
		return &accessError{err.Error()}
	}
	ns := SandboxNamespace(sess.ID, r.cfg)
	api := r.cs.NetworkingV1().NetworkPolicies(ns)
	if want == nil {
		return sandboxRetry("delete sandbox ingress policy", ignoreNotFound(api.Delete(ctx, sandboxIngressPolicy, metav1.DeleteOptions{})))
	}
	have, err := api.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := api.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return sandboxRetry("create sandbox ingress policy", err)
		}
	case err != nil:
		return sandboxRetry("read sandbox ingress policy", err)
	case !reflect.DeepEqual(have.Spec, want.Spec):
		have.Spec = want.Spec
		if _, err := api.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
			return sandboxRetry("update sandbox ingress policy", err)
		}
	}
	return nil
}

// ensureSandboxKubeconfig writes the kubeconfig Secret the pod mounts.
func (r *Reconciler) ensureSandboxKubeconfig(ctx context.Context, sess *store.Session, kubeconfig []byte) error {
	want := BuildSandboxKubeconfig(sess, r.cfg, kubeconfig)
	api := r.cs.CoreV1().Secrets(r.cfg.Namespace)
	have, err := api.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := api.Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create sandbox kubeconfig secret: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read sandbox kubeconfig secret: %w", err)
	case !reflect.DeepEqual(have.Data, want.Data):
		have.Data = want.Data
		if _, err := api.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update sandbox kubeconfig secret: %w", err)
		}
	}
	return nil
}

// removeSandbox deletes a session's sandbox namespace (and with it the
// vcluster and everything that ran in it) and the kubeconfig Secret. It is
// a no-op on hubs without the sandbox mode, which lack the rights to look.
func (r *Reconciler) removeSandbox(ctx context.Context, sess *store.Session, id string) error {
	if err := ignoreNotFound(r.cs.CoreV1().Secrets(r.cfg.Namespace).Delete(ctx, SandboxKubeconfigName(id), metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete sandbox kubeconfig secret: %w", err)
	}
	if !r.cfg.Sandbox.Enabled {
		return nil
	}
	name := SandboxNamespace(id, r.cfg)
	ns, err := r.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read sandbox namespace: %w", err)
	case ns.DeletionTimestamp != nil || ns.Labels[k8s.LabelSandbox] != "true" || ns.Labels[k8s.LabelSession] != id:
		return nil
	}
	if err := ignoreNotFound(r.cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete sandbox namespace: %w", err)
	}
	if sess != nil {
		r.event(ctx, sess, "sandbox", "deleted namespace "+name)
	} else {
		r.log.Info("deleted sandbox namespace", "namespace", name)
	}
	return nil
}

// sandboxSessionIDs lists the sessions that own a sandbox namespace, so
// orphans get visited by ReconcileAll.
func (r *Reconciler) sandboxSessionIDs(ctx context.Context) []string {
	if !r.cfg.Sandbox.Enabled {
		return nil
	}
	list, err := r.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: k8s.LabelSandbox + "=true," + k8s.LabelSession})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		ids = append(ids, ns.Labels[k8s.LabelSession])
	}
	return ids
}

// orphanSandbox removes the sandbox and kubeconfig of a session that no
// longer exists, once they are old enough not to belong to one being created.
func (r *Reconciler) orphanSandbox(ctx context.Context, id string, old func(metav1.Time) bool) error {
	if s, err := r.inf.Secrets.Secrets(r.cfg.Namespace).Get(SandboxKubeconfigName(id)); err == nil && old(s.CreationTimestamp) && s.DeletionTimestamp == nil {
		r.log.Warn("deleting orphaned sandbox kubeconfig", "secret", s.Name)
		if err := ignoreNotFound(r.cs.CoreV1().Secrets(r.cfg.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})); err != nil {
			return err
		}
	}
	if !r.cfg.Sandbox.Enabled {
		return nil
	}
	ns, err := r.cs.CoreV1().Namespaces().Get(ctx, SandboxNamespace(id, r.cfg), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !old(ns.CreationTimestamp):
		return nil
	}
	r.log.Warn("deleting orphaned sandbox namespace", "namespace", ns.Name)
	return r.removeSandbox(ctx, nil, id)
}

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	return slices.ContainsFunc(j.Status.Conditions, func(c batchv1.JobCondition) bool {
		return c.Type == t && c.Status == corev1.ConditionTrue
	})
}

func jobFailure(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return "failed"
}

// sandboxRetryError is a transient sandbox failure: the reconciler retries
// it with backoff and shows it on the session meanwhile. A Forbidden answer
// in the sandbox namespace counts too, since the hub's binding there may
// not have reached the authorizer yet.
type sandboxRetryError struct{ msg string }

func (e *sandboxRetryError) Error() string { return e.msg }

func sandboxRetry(what string, err error) error {
	if err == nil {
		return nil
	}
	return &sandboxRetryError{what + ": " + err.Error()}
}

func isSandboxRetry(err error) (string, bool) {
	var re *sandboxRetryError
	if errors.As(err, &re) {
		return re.msg, true
	}
	return "", false
}
