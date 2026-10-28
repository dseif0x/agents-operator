package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPServer(t *testing.T) {
	var gotOp, gotBody string
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotOp, gotBody = r.URL.Path, string(b)
		if r.URL.Path == "/chat/chat_send" && !strings.Contains(gotBody, `"text":"hi"`) {
			t.Errorf("chat_send body %s", gotBody)
		}
		switch r.URL.Path {
		case "/chat/chat_send":
			_, _ = w.Write([]byte(`{"result":{"id":42}}`))
		case "/chat/chat_read":
			_, _ = w.Write([]byte(`{"result":{"chat":{"name":"launch"},"messages":[{"id":7,"author":{"kind":"session","name":"review","handle":"eager_turing"},"text":"LGTM","reply_to":3,"attachments":[{"id":"a","name":"x.png","content_type":"image/png","path":"/w/home/chats/launch/7-1.png"},{"id":"b","name":"log.txt","content_type":"text/plain; charset=utf-8","path":"/w/home/chats/launch/7-2-log.txt"}],"created_at":"t"}]}}`))
		default:
			_, _ = w.Write([]byte(`{"error":"you are in no chat"}`))
		}
	}))
	defer chat.Close()

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"chat_send","arguments":{"chat":"launch","text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"chat_read","arguments":{"chat":"launch"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"chats_list"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	mcpMain(strings.NewReader(in), &out, chat.URL+"/chat/")
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, r)
	}
	if len(resps) != 7 { // the notification gets no answer
		t.Fatalf("%d responses: %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize = %v", init)
	}
	if tools := resps[1]["result"].(map[string]any)["tools"].([]any); len(tools) != 4 {
		t.Fatalf("tools = %v", tools)
	}
	text := func(i int) (string, bool) {
		r := resps[i]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if s, isErr := text(2); s != "Posted as message #42." || isErr {
		t.Fatalf("chat_send = %q %v (%s %s)", s, isErr, gotOp, gotBody)
	}
	if s, _ := text(3); !strings.Contains(s, "#7 t · review (@eager_turing) · reply to #3\nLGTM\n[image: /w/home/chats/launch/7-1.png]\n[file: /w/home/chats/launch/7-2-log.txt]") {
		t.Fatalf("chat_read = %q", s)
	}
	if s, isErr := text(4); s != "you are in no chat" || !isErr {
		t.Fatalf("chats_list error = %q %v", s, isErr)
	}
	if resps[5]["error"] == nil || resps[6]["error"] == nil {
		t.Fatalf("errors = %v / %v", resps[5], resps[6])
	}
}
