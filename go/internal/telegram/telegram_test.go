package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "123456:SECRET-token"

func server(t *testing.T, handle func(method string, body map[string]any) (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/bot" + testToken + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		code, out := handle(strings.TrimPrefix(r.URL.Path, prefix), body)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	c := New(testToken)
	c.Base = srv.URL
	return c
}

func TestUpdatesAreReadWithTheOffsetAndWait(t *testing.T) {
	var got map[string]any
	c := server(t, func(method string, body map[string]any) (int, string) {
		if method != "getUpdates" {
			t.Errorf("method %s", method)
		}
		got = body
		return 200, `{"ok":true,"result":[
			{"update_id":10,"message":{"message_id":1,"from":{"id":42,"first_name":"Ann"},"chat":{"id":42,"type":"private"},"date":1,"text":"/status"}},
			{"update_id":11,"callback_query":{"id":"cb","from":{"id":42,"first_name":"Ann"},"data":"ap:x","message":{"message_id":2,"chat":{"id":42,"type":"private"},"date":1}}}]}`
	})
	ups, err := c.GetUpdates(context.Background(), 10, 50*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got["offset"].(float64) != 10 || got["timeout"].(float64) != 50 {
		t.Errorf("request = %v", got)
	}
	if len(ups) != 2 || ups[0].Message.Text != "/status" || ups[0].Message.From.ID != 42 ||
		ups[1].CallbackQuery.Data != "ap:x" || ups[1].CallbackQuery.Message.Chat.ID != 42 {
		t.Fatalf("updates = %+v", ups)
	}
}

func TestMessagesGoAsPlainTextWithButtons(t *testing.T) {
	var got map[string]any
	c := server(t, func(method string, body map[string]any) (int, string) {
		got = body
		return 200, `{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"},"date":1}}`
	})
	m, err := c.Send(context.Background(), 42, "a <b>name</b>", Keyboard{{{Text: "Да", Data: "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID != 7 {
		t.Errorf("message id %d", m.MessageID)
	}
	if _, ok := got["parse_mode"]; ok {
		t.Error("a parse mode was set; device names must not be read as markup")
	}
	kb := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if btn := kb[0].([]any)[0].(map[string]any); btn["callback_data"] != "y" || btn["text"] != "Да" {
		t.Errorf("button = %v", btn)
	}
}

func TestAConflictIsRecognised(t *testing.T) {
	c := server(t, func(string, map[string]any) (int, string) {
		return 409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`
	})
	_, err := c.GetUpdates(context.Background(), 0, 0)
	if !IsConflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
}

func TestAnEditThatChangesNothingIsNotAnError(t *testing.T) {
	c := server(t, func(string, map[string]any) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`
	})
	if err := c.Edit(context.Background(), 1, 2, "same", nil); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestTheTokenNeverAppearsInAnError(t *testing.T) {
	c := New(testToken)
	c.Base = "http://127.0.0.1:1" // nothing listens there
	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("no error from an address nobody answers")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "123456") {
		t.Fatalf("the token leaked into the error: %v", err)
	}

	bad := server(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true}` })
	bad.token = "999:WRONG"
	_, err = bad.GetMe(context.Background())
	if !IsUnauthorized(err) || strings.Contains(err.Error(), "WRONG") {
		t.Fatalf("err = %v", err)
	}
}

func TestNameShowsWhoItIs(t *testing.T) {
	u := User{FirstName: "Ann", LastName: "Lee", Username: "ann"}
	if got := u.Name(); got != "Ann Lee (@ann)" {
		t.Errorf("name %q", got)
	}
}
