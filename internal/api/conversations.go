package api

import (
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/dseif0x/agents-operator/internal/runner"
	"github.com/dseif0x/agents-operator/internal/session"
)

// ---- conversations (docs/design/conversations.md) ----

func (s *Server) conversationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/sessions/{id}/items", s.conversationItems)
	mux.HandleFunc("POST /api/v1/sessions/{id}/inputs", s.sendInput)
	mux.HandleFunc("PATCH /api/v1/sessions/{id}/inputs/{input}", s.editInput)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}/inputs/{input}", s.cancelInput)
	mux.HandleFunc("POST /api/v1/sessions/{id}/inputs/{input}/now", s.sendNow)
	mux.HandleFunc("GET /api/v1/sessions/{id}/files/{file}", s.inputFile)
	mux.HandleFunc("POST /api/v1/sessions/{id}/interrupt", s.interrupt)
	mux.HandleFunc("POST /api/v1/sessions/{id}/requests/{request}", s.answerRequest)
	mux.HandleFunc("POST /api/v1/sessions/{id}/model", s.setModel)
	mux.HandleFunc("GET /api/v1/sessions/{id}/memories", s.listMemories)
	mux.HandleFunc("POST /api/v1/sessions/{id}/memories", s.addMemory)
	mux.HandleFunc("PATCH /api/v1/sessions/{id}/memories/{memory}", s.updateMemory)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}/memories/{memory}", s.deleteMemory)
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Sessions.ListMemories(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": ms})
}

func (s *Server) addMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.Sessions.AddMemory(r.Context(), principal(r).User, r.PathValue("id"), body.Text)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.Sessions.UpdateMemory(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("memory"), body.Text)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.DeleteMemory(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("memory")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) answerRequest(w http.ResponseWriter, r *http.Request) {
	var body runner.ConvoResponse
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	it, err := s.Sessions.AnswerRequest(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("request"), body)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) setModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Sessions.SetModel(r.Context(), principal(r).User, r.PathValue("id"), body.Model); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) conversationItems(w http.ResponseWriter, r *http.Request) {
	if turn := r.URL.Query().Get("turn"); turn != "" {
		items, err := s.Sessions.TurnItems(r.Context(), principal(r).User.ID, r.PathValue("id"), turn)
		if err != nil {
			s.mapErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.Sessions.Items(r.Context(), principal(r).User.ID, r.PathValue("id"), before, limit)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) sendInput(w http.ResponseWriter, r *http.Request) {
	var text string
	var files []session.ChatFile
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, int64(session.MaxChatFiles)*session.MaxChatFileBytes+session.MaxInputText+1<<20)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid upload: "+err.Error())
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		text = r.FormValue("text")
		for _, fh := range r.MultipartForm.File["files"] {
			if fh.Size > session.MaxChatFileBytes {
				writeErr(w, http.StatusBadRequest, "files are limited to 10 MiB")
				return
			}
			f, err := fh.Open()
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid upload")
				return
			}
			data, err := io.ReadAll(io.LimitReader(f, session.MaxChatFileBytes+1))
			_ = f.Close()
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid upload")
				return
			}
			files = append(files, session.ChatFile{Name: fh.Filename, Data: data})
		}
	} else {
		var body struct {
			Text string `json:"text"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		text = body.Text
	}
	it, err := s.Sessions.SendInput(r.Context(), principal(r).User, r.PathValue("id"), text, files)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, it)
}

func (s *Server) editInput(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	it, err := s.Sessions.EditInput(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("input"), body.Text)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *Server) cancelInput(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.CancelInput(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("input")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sendNow(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.SendNow(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("input")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.Interrupt(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) inputFile(w http.ResponseWriter, r *http.Request) {
	f, err := s.Sessions.OwnerInputFile(r.Context(), principal(r).User.ID, r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	// As for chat attachments: images inline, anything else a download.
	disposition, ctype := "attachment", "application/octet-stream"
	if session.IsChatImage(f.ContentType) {
		disposition, ctype = "inline", f.ContentType
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if cd := mime.FormatMediaType(disposition, map[string]string{"filename": session.CleanFileName(f.Name)}); cd != "" && f.Name != "" {
		w.Header().Set("Content-Disposition", cd)
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.Data)))
	_, _ = w.Write(f.Data)
}
