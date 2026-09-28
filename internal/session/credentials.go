package session

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/reconcile"
	"github.com/dseif0x/agents-operator/internal/store"
)

// Credentials manages the per-user Secret `agents-operator-user-<id>`. Values live
// only in the Secret; the store records which kinds are set. The hub reads
// the Secret only to project values into session Secrets and never returns
// secret values through the API.
type Credentials struct {
	Store     store.Store
	CS        kubernetes.Interface
	Namespace string
}

// Info is the API view of one credential: which kind is set, when, and the
// value only for non-secret kinds (base URL, git identity).
type Info struct {
	Kind      string    `json:"kind"`
	Secret    bool      `json:"secret"`
	Value     string    `json:"value,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Set stores value under kind in the user's Secret.
func (c *Credentials) Set(ctx context.Context, userID, kind string, value []byte) error {
	if !store.ValidCredentialKind(kind) {
		return &ValidationError{"unknown credential kind " + kind}
	}
	value = normalise(kind, value)
	if len(value) == 0 {
		return &ValidationError{"empty value"}
	}
	name := reconcile.UserSecretName(userID)
	secrets := c.CS.CoreV1().Secrets(c.Namespace)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: map[string]string{
				k8s.LabelUser: userID, k8s.LabelManagedBy: k8s.ManagedBy, k8s.LabelName: "agents-operator", k8s.LabelComponent: "user-credentials",
			}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{kind: value},
		}
		if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create user secret: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read user secret: %w", err)
	default:
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[kind] = value
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update user secret: %w", err)
		}
	}
	return c.Store.Credentials().Upsert(ctx, &store.Credential{UserID: userID, Kind: kind, SecretRef: kind})
}

// Get returns the raw value of one credential kind, or nil when unset. It
// exists for hub-side integrations (the GitHub repository picker); values
// are never returned through the API.
func (c *Credentials) Get(ctx context.Context, userID, kind string) ([]byte, error) {
	if !store.ValidCredentialKind(kind) {
		return nil, &ValidationError{"unknown credential kind " + kind}
	}
	sec, err := c.CS.CoreV1().Secrets(c.Namespace).Get(ctx, reconcile.UserSecretName(userID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read user secret: %w", err)
	}
	return sec.Data[kind], nil
}

// Delete removes kind from the user's Secret.
func (c *Credentials) Delete(ctx context.Context, userID, kind string) error {
	if !store.ValidCredentialKind(kind) {
		return &ValidationError{"unknown credential kind " + kind}
	}
	secrets := c.CS.CoreV1().Secrets(c.Namespace)
	sec, err := secrets.Get(ctx, reconcile.UserSecretName(userID), metav1.GetOptions{})
	if err == nil {
		delete(sec.Data, kind)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update user secret: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read user secret: %w", err)
	}
	return c.Store.Credentials().Delete(ctx, userID, kind)
}

// List describes which credentials the user has. Secret values are never
// included; plain settings are.
func (c *Credentials) List(ctx context.Context, userID string) ([]Info, error) {
	rows, err := c.Store.Credentials().List(ctx, userID)
	if err != nil {
		return nil, err
	}
	var data map[string][]byte
	if sec, err := c.CS.CoreV1().Secrets(c.Namespace).Get(ctx, reconcile.UserSecretName(userID), metav1.GetOptions{}); err == nil {
		data = sec.Data
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		info := Info{Kind: r.Kind, Secret: store.SecretKinds[r.Kind], UpdatedAt: r.UpdatedAt}
		if !info.Secret {
			info.Value = string(data[r.Kind])
		}
		out = append(out, info)
	}
	return out, nil
}

// normalise trims whitespace for single-line values and makes sure key
// material ends with a newline.
func normalise(kind string, v []byte) []byte {
	switch kind {
	case store.CredGitSSHKey:
		s := strings.TrimRight(string(v), "\r\n\t ")
		if s == "" {
			return nil
		}
		return []byte(s + "\n")
	case store.CredClaudeLogin, store.CredCodexLogin:
		return v
	default:
		return []byte(strings.TrimSpace(string(v)))
	}
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
