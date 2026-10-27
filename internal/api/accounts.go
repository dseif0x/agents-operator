package api

import (
	"net/http"

	"github.com/dseif0x/agents-operator/internal/session"
	"github.com/dseif0x/agents-operator/internal/store"
)

// ---- accounts ----
//
// Account values (keys, tokens, SSH keys, logins) go in and never come
// back out: responses describe accounts without them.

func (s *Server) writeAccounts(w http.ResponseWriter, r *http.Request, status int) {
	list, err := s.Creds.ListAccounts(r.Context(), principal(r).User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"accounts": list, "kinds": store.AccountKinds})
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	s.writeAccounts(w, r, http.StatusOK)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body session.AccountInput
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Creds.CreateAccount(r.Context(), principal(r).User.ID, body); err != nil {
		s.mapErr(w, err)
		return
	}
	s.writeAccounts(w, r, http.StatusCreated)
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	var body session.AccountInput
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Creds.UpdateAccount(r.Context(), principal(r).User.ID, r.PathValue("id"), body); err != nil {
		s.mapErr(w, err)
		return
	}
	s.writeAccounts(w, r, http.StatusOK)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.Creds.DeleteAccount(r.Context(), principal(r).User.ID, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	s.writeAccounts(w, r, http.StatusOK)
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.Creds.Profile(r.Context(), principal(r).User.ID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putProfile(w http.ResponseWriter, r *http.Request) {
	var body session.ProfileInfo
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.Creds.PutProfile(r.Context(), principal(r).User.ID, body)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
