package api

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/dseif0x/agents-operator/internal/auth"
)

func TestChatsAPI(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	resp, sess := e.do(t, http.MethodPost, "/api/v1/sessions", map[string]any{"name": "impl", "agent": "claude", "persona": "You implement."}, "hub.test")
	if resp.StatusCode != http.StatusCreated || sess["handle"] == "" || sess["persona"] != "You implement." {
		t.Fatalf("session = %d %v", resp.StatusCode, sess)
	}
	sid, handle := sess["id"].(string), sess["handle"].(string)

	resp, chat := e.do(t, http.MethodPost, "/api/v1/chats", map[string]any{"name": "launch", "members": []string{sid}}, "hub.test")
	if resp.StatusCode != http.StatusCreated || len(chat["members"].([]any)) != 1 {
		t.Fatalf("chat = %d %v", resp.StatusCode, chat)
	}
	cid := chat["id"].(string)

	resp, msg := e.do(t, http.MethodPost, "/api/v1/chats/"+cid+"/messages", map[string]any{"text": "hi @" + handle}, "hub.test")
	if resp.StatusCode != http.StatusCreated || msg["deliveries"].(map[string]any)[sid] != "queued" {
		t.Fatalf("message = %d %v", resp.StatusCode, msg)
	}

	// An image, as multipart.
	var img bytes.Buffer
	_ = png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("text", "screenshot")
	fw, _ := mw.CreateFormFile("images", "shot.png")
	_, _ = fw.Write(img.Bytes())
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/chats/"+cid+"/messages", &body)
	req.Host = "hub.test"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(auth.CSRFHeader, e.csrf)
	r2, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r2.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(r2.Body)
		t.Fatalf("upload = %d %s", r2.StatusCode, b)
	}
	_ = r2.Body.Close()

	resp, list := e.do(t, http.MethodGet, "/api/v1/chats/"+cid+"/messages", nil, "hub.test")
	msgs := list["messages"].([]any)
	if resp.StatusCode != http.StatusOK || len(msgs) != 2 {
		t.Fatalf("messages = %d %v", resp.StatusCode, list)
	}
	att := msgs[1].(map[string]any)["attachments"].([]any)[0].(map[string]any)
	ireq, _ := http.NewRequest(http.MethodGet, e.srv.URL+att["url"].(string), nil)
	ireq.Host = "hub.test"
	ir, err := e.client.Do(ireq)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(ir.Body)
	_ = ir.Body.Close()
	if ir.StatusCode != http.StatusOK || ir.Header.Get("Content-Type") != "image/png" || ir.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(got, img.Bytes()) {
		t.Fatalf("image = %d %v", ir.StatusCode, ir.Header)
	}

	// Rename, mute a member, read marker, list, delete.
	if resp, c := e.do(t, http.MethodPatch, "/api/v1/chats/"+cid, map[string]any{"name": "launch-2", "description": "ship"}, "hub.test"); resp.StatusCode != http.StatusOK || c["name"] != "launch-2" {
		t.Fatalf("patch = %d %v", resp.StatusCode, c)
	}
	if resp, c := e.do(t, http.MethodPatch, "/api/v1/chats/"+cid+"/members/"+sid, map[string]any{"muted": true}, "hub.test"); resp.StatusCode != http.StatusOK || !c["members"].([]any)[0].(map[string]any)["muted"].(bool) {
		t.Fatalf("mute = %d %v", resp.StatusCode, c)
	}
	if resp, _ := e.do(t, http.MethodPost, "/api/v1/chats/"+cid+"/read", map[string]any{"last_id": 2}, "hub.test"); resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d", resp.StatusCode)
	}
	if resp, l := e.do(t, http.MethodGet, "/api/v1/chats", nil, "hub.test"); resp.StatusCode != http.StatusOK || len(l["chats"].([]any)) != 1 {
		t.Fatalf("list = %d %v", resp.StatusCode, l)
	}
	if resp, _ := e.do(t, http.MethodGet, "/api/v1/chats/nope", nil, "hub.test"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing chat = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, http.MethodDelete, "/api/v1/chats/"+cid, nil, "hub.test"); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
}
