package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/auth"
	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
)

// ---- routines (docs/design/routines.md) ----

func (s *Server) routineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/routines", s.listRoutines)
	mux.HandleFunc("POST /api/v1/routines", s.createRoutine)
	mux.HandleFunc("GET /api/v1/routines/calendar", s.routineCalendar)
	mux.HandleFunc("PATCH /api/v1/routines/{id}", s.updateRoutine)
	mux.HandleFunc("DELETE /api/v1/routines/{id}", s.deleteRoutine)
	mux.HandleFunc("POST /api/v1/routines/{id}/run", s.runRoutine)
	mux.HandleFunc("POST /api/v1/routines/{id}/secret", s.rotateSecret)
	mux.HandleFunc("GET /api/v1/routines/{id}/runs", s.routineRuns)
}

func (s *Server) listRoutines(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Sessions.ListRoutines(r.Context(), principal(r).User, r.URL.Query().Get("session"), r.URL.Query().Get("chat"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routines": rs})
}

func (s *Server) createRoutine(w http.ResponseWriter, r *http.Request) {
	var in session.RoutineInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.Sessions.CreateRoutine(r.Context(), principal(r).User, in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) updateRoutine(w http.ResponseWriter, r *http.Request) {
	var in session.RoutineInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.Sessions.UpdateRoutine(r.Context(), principal(r).User, r.PathValue("id"), in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteRoutine(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.DeleteRoutine(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) runRoutine(w http.ResponseWriter, r *http.Request) {
	run, err := s.Sessions.RunRoutineNow(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request) {
	v, err := s.Sessions.RotateSecret(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) routineRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.Sessions.RoutineRuns(r.Context(), principal(r).User, r.PathValue("id"), limit)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) routineCalendar(w http.ResponseWriter, r *http.Request) {
	from, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) {
		writeErr(w, http.StatusBadRequest, "from and to are RFC 3339 times, from before to")
		return
	}
	entries, err := s.Sessions.Calendar(r.Context(), principal(r).User, from, to)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// webhook is the public endpoint of webhook routines: no login, the
// request is signed with the routine's secret (X-Hub-Signature-256 or
// X-Signature-256: sha256=<HMAC of the body>) or carries it as a bearer
// token or ?token=.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	ip := auth.ClientIP(r)
	if !s.Limiter.Allowed("hook:" + ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed requests; try again later")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, session.MaxWebhookBody+1))
	if err != nil || len(body) > session.MaxWebhookBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "the body is limited to 1 MiB")
		return
	}
	req := session.WebhookRequest{
		RoutineID: r.PathValue("id"),
		Body:      body,
		Signature: firstHeader(r, "X-Hub-Signature-256", "X-Signature-256"),
		Token:     r.URL.Query().Get("token"),
		EventType: firstHeader(r, "X-Event-Type", "X-GitHub-Event", "X-Gitlab-Event", "X-Event-Key"),
	}
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		req.Token = t
	}
	run, ignored, err := s.Sessions.Webhook(r.Context(), req)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, session.ErrWebhookAuth):
		// One answer for both, so the endpoint does not tell which routines exist.
		s.Limiter.Fail("hook:" + ip)
		writeErr(w, http.StatusUnauthorized, "unknown routine or bad signature")
	case err != nil:
		s.mapErr(w, err)
	case ignored != "":
		writeJSON(w, http.StatusAccepted, map[string]string{"ignored": ignored})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"run": run.ID, "status": run.Status})
	}
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}
