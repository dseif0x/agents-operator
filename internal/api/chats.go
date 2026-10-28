package api

import (
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/dseif0x/agents-operator/internal/session"
)

// ---- chats ----

func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/chats", s.listChats)
	mux.HandleFunc("POST /api/v1/chats", s.createChat)
	mux.HandleFunc("GET /api/v1/chats/{id}", s.getChat)
	mux.HandleFunc("PATCH /api/v1/chats/{id}", s.updateChat)
	mux.HandleFunc("DELETE /api/v1/chats/{id}", s.deleteChat)
	mux.HandleFunc("POST /api/v1/chats/{id}/members", s.addChatMember)
	mux.HandleFunc("PATCH /api/v1/chats/{id}/members/{session}", s.updateChatMember)
	mux.HandleFunc("DELETE /api/v1/chats/{id}/members/{session}", s.removeChatMember)
	mux.HandleFunc("GET /api/v1/chats/{id}/messages", s.chatMessages)
	mux.HandleFunc("POST /api/v1/chats/{id}/messages", s.postChatMessage)
	mux.HandleFunc("POST /api/v1/chats/{id}/read", s.markChatRead)
	mux.HandleFunc("GET /api/v1/chats/{id}/attachments/{aid}", s.chatAttachment)
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sessions.ListChats(r.Context(), principal(r).User)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": list})
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var in session.ChatInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.Sessions.CreateChat(r.Context(), principal(r).User, in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	c, err := s.Sessions.GetChat(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) updateChat(w http.ResponseWriter, r *http.Request) {
	var in session.ChatInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.Sessions.UpdateChat(r.Context(), principal(r).User, r.PathValue("id"), in)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.DeleteChat(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) addChatMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.Sessions.AddChatMember(r.Context(), principal(r).User, r.PathValue("id"), body.SessionID)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) updateChatMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Muted bool `json:"muted"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.Sessions.SetChatMemberMuted(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("session"), body.Muted)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) removeChatMember(w http.ResponseWriter, r *http.Request) {
	c, err := s.Sessions.RemoveChatMember(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("session"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	msgs, err := s.Sessions.ChatMessages(r.Context(), principal(r).User, r.PathValue("id"), before, after, limit)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// postChatMessage takes JSON {"text", "reply_to"} or, with files,
// multipart/form-data with fields text and reply_to and the files under
// "files" (or "images", the name before files of any type were allowed).
func (s *Server) postChatMessage(w http.ResponseWriter, r *http.Request) {
	var text string
	var replyTo int64
	var images []session.ChatFile
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, int64(session.MaxChatFiles)*session.MaxChatFileBytes+session.MaxChatText+1<<20)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid upload: "+err.Error())
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		text = r.FormValue("text")
		replyTo, _ = strconv.ParseInt(r.FormValue("reply_to"), 10, 64)
		uploads := append(r.MultipartForm.File["files"], r.MultipartForm.File["images"]...)
		for _, fh := range uploads {
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
			images = append(images, session.ChatFile{Name: fh.Filename, Data: data})
		}
	} else {
		var body struct {
			Text    string `json:"text"`
			ReplyTo int64  `json:"reply_to"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		text, replyTo = body.Text, body.ReplyTo
	}
	m, err := s.Sessions.PostChatMessage(r.Context(), principal(r).User, r.PathValue("id"), text, replyTo, images)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) markChatRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LastID int64 `json:"last_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Sessions.MarkChatRead(r.Context(), principal(r).User, r.PathValue("id"), body.LastID); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) chatAttachment(w http.ResponseWriter, r *http.Request) {
	a, data, err := s.Sessions.ChatAttachment(r.Context(), principal(r).User, r.PathValue("id"), r.PathValue("aid"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	// Images (sniffed on upload) are shown inline; every other file is a
	// download of opaque bytes, so a browser never renders an uploaded HTML
	// page or script from the hub's origin. Never let a browser guess.
	disposition, ctype := "attachment", "application/octet-stream"
	if session.IsChatImage(a.ContentType) {
		disposition, ctype = "inline", a.ContentType
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if cd := mime.FormatMediaType(disposition, map[string]string{"filename": session.CleanFileName(a.Name)}); cd != "" && a.Name != "" {
		w.Header().Set("Content-Disposition", cd)
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}
