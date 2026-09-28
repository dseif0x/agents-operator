package reconcile

import (
	"context"
	"errors"

	"github.com/dseif0x/agents-operator/internal/k8s"
	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/term"
)

// Resolver finds a session's runner endpoint from the informer caches: the
// pod IP and the runner token from the per-session Secret.
type Resolver struct {
	Informers *k8s.Informers
	Namespace string
}

// Endpoint implements term.Resolver.
func (r *Resolver) Endpoint(_ context.Context, sessionID string) (term.Endpoint, error) {
	name := ObjectName(sessionID)
	ip, ok := r.Informers.PodIP(r.Namespace, name)
	if !ok {
		return term.Endpoint{}, term.ErrNotRunning
	}
	sec, err := r.Informers.Secrets.Secrets(r.Namespace).Get(name)
	if err != nil {
		return term.Endpoint{}, errors.New("session secret not found")
	}
	tok := string(sec.Data[runner.EnvRunnerToken])
	if tok == "" {
		return term.Endpoint{}, errors.New("session secret has no runner token")
	}
	return term.Endpoint{IP: ip, Port: runner.Port, Token: tok}, nil
}

var _ term.Resolver = (*Resolver)(nil)
