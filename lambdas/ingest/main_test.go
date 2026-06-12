package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/joshhwuu/recall/pkg/store"
)

type fakeStore struct {
	puts       []*store.Note
	existingID string // returned by PutNoteIdempotent to simulate a replay
	sessions   map[string]store.Session
}

func (f *fakeStore) PutNote(_ context.Context, n *store.Note) error {
	f.puts = append(f.puts, n)
	return nil
}

func (f *fakeStore) PutNoteIdempotent(_ context.Context, n *store.Note, _ string) (string, error) {
	if f.existingID != "" {
		return f.existingID, nil
	}
	f.puts = append(f.puts, n)
	return "", nil
}

func (f *fakeStore) CreateSession(_ context.Context, tokenHash string, sess store.Session) error {
	if f.sessions == nil {
		f.sessions = map[string]store.Session{}
	}
	f.sessions[tokenHash] = sess
	return nil
}

func (f *fakeStore) GetSession(_ context.Context, tokenHash string) (*store.Session, error) {
	sess, ok := f.sessions[tokenHash]
	if !ok || sess.ExpiresAt <= time.Now().Unix() {
		return nil, nil
	}
	return &sess, nil
}

const testToken = "test-token-123"

func newHandler(fs *fakeStore) *handler {
	return &handler{
		store:       fs,
		staticToken: testToken,
		staticUser:  "joshua",
		verifyGoogle: func(_ context.Context, credential string) (string, string, error) {
			if credential == "good-credential" {
				return "google-sub-42", "anyone@example.com", nil
			}
			return "", "", errors.New("bad credential")
		},
		html:     indexHTML,
		sessions: map[string]*store.Session{},
	}
}

func request(method, path, auth, body string, extra map[string]string) events.APIGatewayV2HTTPRequest {
	headers := map[string]string{}
	if auth != "" {
		headers["authorization"] = auth
	}
	for k, v := range extra {
		headers[k] = v
	}
	req := events.APIGatewayV2HTTPRequest{
		RawPath: path,
		Headers: headers,
		Body:    body,
	}
	req.RequestContext.HTTP.Method = method
	return req
}

func TestRejectsMissingAndWrongToken(t *testing.T) {
	h := newHandler(&fakeStore{})
	for _, auth := range []string{"", "Bearer wrong", testToken /* missing Bearer prefix */} {
		resp, err := h.Handle(context.Background(), request("POST", "/entries", auth, `{"text":"x"}`, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 401 {
			t.Errorf("auth %q: status = %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestRejectsBadBody(t *testing.T) {
	h := newHandler(&fakeStore{})
	for name, body := range map[string]string{
		"empty text": `{"text":"  "}`,
		"not json":   `hello`,
		"too long":   `{"text":"` + strings.Repeat("a", maxTextLen+1) + `"}`,
	} {
		resp, _ := h.Handle(context.Background(), request("POST", "/entries", "Bearer "+testToken, body, nil))
		if resp.StatusCode != 400 {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestStaticTokenWritesToStaticUser(t *testing.T) {
	fs := &fakeStore{}
	h := newHandler(fs)
	resp, _ := h.Handle(context.Background(),
		request("POST", "/entries", "Bearer "+testToken, `{"text":"jaden bday march 12"}`, nil))
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201; body %s", resp.StatusCode, resp.Body)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil || out["id"] == "" {
		t.Fatalf("body %q: want {id}", resp.Body)
	}
	if len(fs.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(fs.puts))
	}
	n := fs.puts[0]
	if n.RawText != "jaden bday march 12" || n.Source != "web" || n.Enriched {
		t.Errorf("note = %+v", n)
	}
	if n.PK != "USER#joshua" || !strings.HasPrefix(n.SK, "NOTE#") {
		t.Errorf("keys = %s / %s", n.PK, n.SK)
	}
}

func TestGoogleSignInThenCapture(t *testing.T) {
	fs := &fakeStore{}
	h := newHandler(fs)

	resp, _ := h.Handle(context.Background(),
		request("POST", "/auth/google", "", `{"credential":"good-credential"}`, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("auth status = %d, want 200; body %s", resp.StatusCode, resp.Body)
	}
	var auth map[string]string
	json.Unmarshal([]byte(resp.Body), &auth)
	token := auth["token"]
	if len(token) != 64 {
		t.Fatalf("token %q: want 64 hex chars", token)
	}
	if _, raw := fs.sessions[token]; raw {
		t.Error("session stored under raw token; must be stored under its hash")
	}
	if sess, ok := fs.sessions[hashToken(token)]; !ok {
		t.Fatal("no session stored under hashed token")
	} else if sess.UserID != "google-sub-42" {
		t.Errorf("session UserID = %q, want google-sub-42", sess.UserID)
	}

	resp, _ = h.Handle(context.Background(),
		request("POST", "/entries", "Bearer "+token, `{"text":"my first note"}`, nil))
	if resp.StatusCode != 201 {
		t.Fatalf("entry status = %d, want 201; body %s", resp.StatusCode, resp.Body)
	}
	if len(fs.puts) != 1 || fs.puts[0].PK != "USER#google-sub-42" {
		t.Errorf("note written to %q, want USER#google-sub-42", fs.puts[0].PK)
	}
}

func TestRejectsBadGoogleCredential(t *testing.T) {
	h := newHandler(&fakeStore{})
	resp, _ := h.Handle(context.Background(),
		request("POST", "/auth/google", "", `{"credential":"garbage"}`, nil))
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	resp, _ = h.Handle(context.Background(),
		request("POST", "/auth/google", "", `{}`, nil))
	if resp.StatusCode != 400 {
		t.Errorf("empty credential: status = %d, want 400", resp.StatusCode)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	fs := &fakeStore{sessions: map[string]store.Session{
		hashToken("expired-token"): {UserID: "u", ExpiresAt: time.Now().Add(-time.Hour).Unix()},
	}}
	h := newHandler(fs)
	resp, _ := h.Handle(context.Background(),
		request("POST", "/entries", "Bearer expired-token", `{"text":"x"}`, nil))
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestIdempotentReplayReturns200WithOriginalID(t *testing.T) {
	fs := &fakeStore{existingID: "01ORIGINAL"}
	h := newHandler(fs)
	resp, _ := h.Handle(context.Background(),
		request("POST", "/entries", "Bearer "+testToken, `{"text":"hi"}`,
			map[string]string{"idempotency-key": "abc"}))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	json.Unmarshal([]byte(resp.Body), &out)
	if out["id"] != "01ORIGINAL" {
		t.Errorf("id = %q, want 01ORIGINAL", out["id"])
	}
	if len(fs.puts) != 0 {
		t.Errorf("replay wrote %d notes, want 0", len(fs.puts))
	}
}

func TestServesCapturePage(t *testing.T) {
	h := newHandler(&fakeStore{})
	resp, _ := h.Handle(context.Background(), request("GET", "/", "", "", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Body, "<textarea") {
		t.Error("page missing capture textarea")
	}
}
