package api

import (
	"net/http"

	"github.com/dseif0x/agents-operator/internal/session"
)

func (s *Server) routesSkills(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me/skills", s.listSkills)
	mux.HandleFunc("POST /api/v1/me/skills", s.createSkill)
	mux.HandleFunc("PUT /api/v1/me/skills/{id}", s.updateSkill)
	mux.HandleFunc("DELETE /api/v1/me/skills/{id}", s.deleteSkill)
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sessions.ListSkills(r.Context(), principal(r).User)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": list})
}

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var in session.SkillInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sk, err := s.Sessions.CreateSkill(r.Context(), principal(r).User, in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	var in session.SkillInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sk, err := s.Sessions.UpdateSkill(r.Context(), principal(r).User, r.PathValue("id"), in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.DeleteSkill(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
