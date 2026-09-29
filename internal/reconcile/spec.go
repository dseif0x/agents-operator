// Package reconcile turns session rows into Kubernetes objects and keeps
// them converged. The spec builders in this file are pure functions so they
// can be unit-tested without a cluster.
package reconcile

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/dseif0x/agents-operator/internal/config"
	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Config is what the reconciler needs from the hub configuration.
type Config struct {
	Namespace           string
	RunnerImage         string // repository without tag
	RunnerImageTag      string
	ImagePullPolicy     string
	DefaultStorageClass string
	DefaultPVCSize      string
	DefaultResources    config.Resources
	NodeSelector        map[string]string
	Tolerations         []config.Toleration
	RuntimeClass        string
	ExtraEnv            map[string]string
	// OrphanGrace is how old a labelled object without a row must be
	// before it is deleted.
	OrphanGrace time.Duration
	// CreatingTimeout marks a session failed when its pod has not become
	// ready in time.
	CreatingTimeout time.Duration
}

// Defaults fills unset durations.
func (c Config) Defaults() Config {
	if c.OrphanGrace == 0 {
		c.OrphanGrace = 2 * time.Minute
	}
	if c.CreatingTimeout == 0 {
		c.CreatingTimeout = 15 * time.Minute
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	if c.RunnerImageTag == "" {
		c.RunnerImageTag = "latest"
	}
	return c
}

const (
	// RunnerUID is the non-root user every session pod runs as.
	RunnerUID int64 = 1000
	// ContainerName is the single container in a session pod.
	ContainerName = "runner"
	// WorkspacePath is where the PVC is mounted.
	WorkspacePath = "/workspace"
)

// ObjectName is the name shared by a session's PVC, Secret and Pod.
func ObjectName(sessionID string) string { return "agents-operator-" + sessionID }

// UserSecretName is the per-user credential Secret.
func UserSecretName(userID string) string { return "agents-operator-user-" + userID }

// SessionIDFromName is the inverse of ObjectName.
func SessionIDFromName(name string) (string, bool) {
	return strings.CutPrefix(name, "agents-operator-")
}

// Labels returns the labels every per-session object carries.
func Labels(s *store.Session) map[string]string {
	return map[string]string{
		k8s.LabelSession:   s.ID,
		k8s.LabelOwner:     s.OwnerID,
		k8s.LabelManagedBy: k8s.ManagedBy,
		k8s.LabelName:      "agents-operator-runner",
		k8s.LabelComponent: "session",
	}
}

// BuildPVC returns the desired PersistentVolumeClaim.
func BuildPVC(s *store.Session, cfg Config) *corev1.PersistentVolumeClaim {
	size := s.PVCSize
	if size == "" {
		size = cfg.DefaultPVCSize
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ObjectName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	sc := s.StorageClass
	if sc == "" {
		sc = cfg.DefaultStorageClass
	}
	if sc != "" {
		pvc.Spec.StorageClassName = ptr.To(sc)
	}
	return pvc
}

// credentialEnv maps user credential kinds to the env var the runner reads.
var credentialEnv = map[string]string{
	store.CredAnthropicAPIKey:  "ANTHROPIC_API_KEY",
	store.CredAnthropicBaseURL: "ANTHROPIC_BASE_URL",
	store.CredClaudeOAuthToken: runner.EnvClaudeOAuthToken,
	store.CredOpenAIAPIKey:     "OPENAI_API_KEY",
	store.CredGitSSHKey:        runner.EnvGitSSHKey,
	store.CredGitHTTPSToken:    runner.EnvGitHTTPSToken,
	store.CredGitHubToken:      runner.EnvGitHubToken,
	store.CredGitUserName:      runner.EnvGitUserName,
	store.CredGitUserEmail:     runner.EnvGitUserEmail,
}

// BuildSecret returns the per-session Secret: the runner token plus the
// user's credentials projected as env. userSecret is the data of the
// per-user Secret (may be nil).
func BuildSecret(s *store.Session, cfg Config, token string, userSecret map[string][]byte) *corev1.Secret {
	data := map[string][]byte{runner.EnvRunnerToken: []byte(token)}
	for kind, env := range credentialEnv {
		if v, ok := userSecret[kind]; ok && len(v) > 0 {
			data[env] = v
		}
	}
	// Saved CLI logins are seeded on first boot; the runner expects base64.
	for kind := range runner.LoginFiles {
		if v, ok := userSecret[kind]; ok && len(v) > 0 {
			data[runner.EnvSeedPrefix+strings.ToUpper(kind)] = []byte(base64.StdEncoding.EncodeToString(v))
		}
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ObjectName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s),
			Annotations: map[string]string{k8s.AnnotationGeneration: strconv.Itoa(s.Generation)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// ClampResources applies the session's overrides on top of the defaults.
// The chart default is the ceiling: a session can only lower limits, and
// requests can never exceed limits.
func ClampResources(want, def config.Resources) corev1.ResourceRequirements {
	limitCPU := minQty(want.Limits.CPU, def.Limits.CPU)
	limitMem := minQty(want.Limits.Memory, def.Limits.Memory)
	reqCPU := minQty(firstNonEmpty(want.Requests.CPU, def.Requests.CPU), limitCPU)
	reqMem := minQty(firstNonEmpty(want.Requests.Memory, def.Requests.Memory), limitMem)
	out := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	set := func(list corev1.ResourceList, name corev1.ResourceName, v string) {
		if v == "" {
			return
		}
		if q, err := resource.ParseQuantity(v); err == nil {
			list[name] = q
		}
	}
	set(out.Requests, corev1.ResourceCPU, reqCPU)
	set(out.Requests, corev1.ResourceMemory, reqMem)
	set(out.Limits, corev1.ResourceCPU, limitCPU)
	set(out.Limits, corev1.ResourceMemory, limitMem)
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// minQty returns the smaller of two quantities; an empty or unparsable
// side yields the other.
func minQty(a, b string) string {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	switch {
	case errA != nil && errB != nil:
		return ""
	case errA != nil:
		return b
	case errB != nil:
		return a
	}
	if qa.Cmp(qb) <= 0 {
		return a
	}
	return b
}

var dnsLabelRE = regexp.MustCompile(`[^a-z0-9-]+`)

// Hostname turns a session name into a valid DNS label.
func Hostname(name string) string {
	h := strings.ToLower(strings.TrimSpace(name))
	h = dnsLabelRE.ReplaceAllString(h, "-")
	h = strings.Trim(h, "-")
	if len(h) > 63 {
		h = strings.Trim(h[:63], "-")
	}
	if h == "" {
		h = "session"
	}
	return h
}

// BuildPod returns the desired Pod. Every security setting here is a hard
// requirement; there is deliberately no knob to relax them.
func BuildPod(s *store.Session, cfg Config) *corev1.Pod {
	cfg = cfg.Defaults()
	tag := s.ImageTag
	if tag == "" {
		tag = cfg.RunnerImageTag
	}
	repos := s.Repos
	if repos == nil {
		repos = []store.Repo{}
	}
	reposJSON, _ := json.Marshal(repos)
	env := []corev1.EnvVar{
		{Name: runner.EnvAgent, Value: s.Agent},
		{Name: runner.EnvAutonomous, Value: strconv.FormatBool(s.Autonomous)},
		{Name: runner.EnvRepos, Value: string(reposJSON)},
		{Name: runner.EnvSessionName, Value: s.Name},
		{Name: runner.EnvWorkspace, Value: WorkspacePath},
		{Name: "HOME", Value: WorkspacePath + "/home"},
	}
	for _, k := range sortedKeys(cfg.ExtraEnv) {
		env = append(env, corev1.EnvVar{Name: k, Value: cfg.ExtraEnv[k]})
	}
	for _, k := range sortedKeys(s.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: s.Env[k]})
	}

	nodeSelector := map[string]string{}
	for k, v := range cfg.NodeSelector {
		nodeSelector[k] = v
	}
	for k, v := range s.NodeSelector {
		nodeSelector[k] = v
	}
	var tolerations []corev1.Toleration
	for _, t := range append(append([]config.Toleration{}, cfg.Tolerations...), s.Tolerations...) {
		tolerations = append(tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: ObjectName(s.ID), Namespace: cfg.Namespace, Labels: Labels(s),
			Annotations: map[string]string{k8s.AnnotationGeneration: strconv.Itoa(s.Generation)},
		},
		Spec: corev1.PodSpec{
			Hostname:                      Hostname(s.Name),
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To[int64](30),
			NodeSelector:                  nodeSelector,
			Tolerations:                   tolerations,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(RunnerUID),
				RunAsGroup:     ptr.To(RunnerUID),
				FSGroup:        ptr.To(RunnerUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            ContainerName,
				Image:           cfg.RunnerImage + ":" + tag,
				ImagePullPolicy: corev1.PullPolicy(cfg.ImagePullPolicy),
				Ports:           []corev1.ContainerPort{{Name: "ws", ContainerPort: runner.Port, Protocol: corev1.ProtocolTCP}},
				Env:             env,
				EnvFrom:         []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: ObjectName(s.ID)}}}},
				Resources:       ClampResources(s.Resources, cfg.DefaultResources),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: WorkspacePath},
					{Name: "tmp", MountPath: "/tmp"},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					RunAsNonRoot:             ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrFromInt(runner.Port)}},
					InitialDelaySeconds: 2, PeriodSeconds: 5, FailureThreshold: 3,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrFromInt(runner.Port)}},
					InitialDelaySeconds: 30, PeriodSeconds: 20, FailureThreshold: 3,
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ObjectName(s.ID)}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	if cfg.RuntimeClass != "" {
		pod.Spec.RuntimeClassName = ptr.To(cfg.RuntimeClass)
	}
	return pod
}

// PodReady reports whether the pod is Running with Ready=True.
func PodReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// PodTerminal reports whether the pod has finished for good.
func PodTerminal(pod *corev1.Pod) bool {
	return pod != nil && (pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded)
}

// PodReason summarises why a pod is not ready, for the state_reason column.
func PodReason(pod *corev1.Pod) string {
	if pod == nil {
		return "pod missing"
	}
	if pod.DeletionTimestamp != nil {
		return "pod terminating"
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return fmt.Sprintf("%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil {
			return fmt.Sprintf("container exited with code %d (%s)", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason + ": " + pod.Status.Message
	}
	return "pod " + strings.ToLower(string(pod.Status.Phase))
}
