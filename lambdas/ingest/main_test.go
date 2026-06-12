package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/joshhwuu/recall/pkg/store"
)

type fakeStore struct {
	puts       []*store.Note
	existingID string // returned by PutNoteIdempotent to simulate a replay
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

const testToken = "test-token-123"

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
	h := &handler{store: &fakeStore{}, token: testToken}
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
	h := &handler{store: &fakeStore{}, token: testToken}
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

func TestCreatesNote(t *testing.T) {
	fs := &fakeStore{}
	h := &handler{store: fs, token: testToken}
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

func TestIdempotentReplayReturns200WithOriginalID(t *testing.T) {
	fs := &fakeStore{existingID: "01ORIGINAL"}
	h := &handler{store: fs, token: testToken}
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
	h := &handler{store: &fakeStore{}, token: testToken}
	resp, _ := h.Handle(context.Background(), request("GET", "/", "", "", nil))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(resp.Body, "<textarea") {
		t.Error("page missing capture textarea")
	}
}
