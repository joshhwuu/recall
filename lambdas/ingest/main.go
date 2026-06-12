// Command ingest is the capture-path Lambda behind the recall HTTP API.
// POST /entries writes a raw, unenriched note to Dynamo, POST /auth/google
// exchanges a Google ID token for a recall session, and GET / serves the
// embedded capture page. No AI runs in this path (see PLAN.md, Phase 2).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/oklog/ulid/v2"
	"google.golang.org/api/idtoken"

	"github.com/joshhwuu/recall/pkg/store"
)

//go:embed index.html
var indexHTML string

const (
	maxTextLen = 10 * 1024
	sessionTTL = 90 * 24 * time.Hour
)

type noteStore interface {
	PutNote(ctx context.Context, n *store.Note) error
	PutNoteIdempotent(ctx context.Context, n *store.Note, idemKey string) (existingID string, err error)
	CreateSession(ctx context.Context, tokenHash string, sess store.Session) error
	GetSession(ctx context.Context, tokenHash string) (*store.Session, error)
}

// verifyGoogleFunc validates a Google ID token and returns the account's
// stable sub claim and email.
type verifyGoogleFunc func(ctx context.Context, credential string) (sub, email string, err error)

type handler struct {
	store        noteStore
	staticToken  string
	staticUser   string // partition for static-token writes (iOS Shortcut)
	verifyGoogle verifyGoogleFunc
	html         string

	mu       sync.Mutex
	sessions map[string]*store.Session // verified token-hash → session, warm-invocation cache
}

type entryRequest struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

type authRequest struct {
	Credential string `json:"credential"`
}

func (h *handler) Handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	method := req.RequestContext.HTTP.Method
	path := req.RawPath
	switch {
	case method == "GET" && (path == "/" || path == ""):
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8"},
			Body:       h.html,
		}, nil
	case method == "POST" && path == "/entries":
		return h.postEntry(ctx, req), nil
	case method == "POST" && path == "/auth/google":
		return h.postAuthGoogle(ctx, req), nil
	default:
		return jsonResp(404, map[string]string{"error": "not found"}), nil
	}
}

func (h *handler) postAuthGoogle(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	var body authRequest
	if err := json.Unmarshal([]byte(requestBody(req)), &body); err != nil || body.Credential == "" {
		return jsonResp(400, map[string]string{"error": "credential is required"})
	}
	sub, email, err := h.verifyGoogle(ctx, body.Credential)
	if err != nil {
		log.Printf("google verify: %v", err)
		return jsonResp(401, map[string]string{"error": "invalid Google credential"})
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return jsonResp(500, map[string]string{"error": "token generation failed"})
	}
	token := hex.EncodeToString(raw)
	expires := time.Now().Add(sessionTTL)
	sess := store.Session{UserID: sub, Email: email, ExpiresAt: expires.Unix()}
	if err := h.store.CreateSession(ctx, hashToken(token), sess); err != nil {
		log.Printf("create session: %v", err)
		return jsonResp(500, map[string]string{"error": "session creation failed"})
	}
	return jsonResp(200, map[string]string{
		"token":      token,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func (h *handler) postEntry(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	userID, ok := h.resolveUser(ctx, req)
	if !ok {
		return jsonResp(401, map[string]string{"error": "unauthorized"})
	}

	var body entryRequest
	if err := json.Unmarshal([]byte(requestBody(req)), &body); err != nil {
		return jsonResp(400, map[string]string{"error": "invalid JSON body"})
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		return jsonResp(400, map[string]string{"error": "text is required"})
	}
	if len(text) > maxTextLen {
		return jsonResp(400, map[string]string{"error": "text too long"})
	}
	source := body.Source
	if source == "" {
		source = "web"
	}

	id := ulid.Make().String()
	n := &store.Note{
		Key:       store.NoteKey(userID, id),
		ID:        id,
		RawText:   text,
		Source:    source,
		Enriched:  false,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if idemKey := header(req, "idempotency-key"); idemKey != "" {
		existingID, err := h.store.PutNoteIdempotent(ctx, n, idemKey)
		if err != nil {
			log.Printf("put note (idempotent): %v", err)
			return jsonResp(500, map[string]string{"error": "write failed"})
		}
		if existingID != "" {
			return jsonResp(200, map[string]string{"id": existingID})
		}
		return jsonResp(201, map[string]string{"id": id})
	}

	if err := h.store.PutNote(ctx, n); err != nil {
		log.Printf("put note: %v", err)
		return jsonResp(500, map[string]string{"error": "write failed"})
	}
	return jsonResp(201, map[string]string{"id": id})
}

// resolveUser maps the request's bearer token to a user partition: the
// static token maps to staticUser, anything else must be a live session.
func (h *handler) resolveUser(ctx context.Context, req events.APIGatewayV2HTTPRequest) (string, bool) {
	auth := header(req, "authorization")
	bearer, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || bearer == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(h.staticToken)) == 1 {
		return h.staticUser, true
	}

	hash := hashToken(bearer)
	h.mu.Lock()
	cached := h.sessions[hash]
	h.mu.Unlock()
	if cached != nil && cached.ExpiresAt > time.Now().Unix() {
		return cached.UserID, true
	}

	sess, err := h.store.GetSession(ctx, hash)
	if err != nil {
		log.Printf("get session: %v", err)
		return "", false
	}
	if sess == nil {
		return "", false
	}
	h.mu.Lock()
	h.sessions[hash] = sess
	h.mu.Unlock()
	return sess.UserID, true
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func requestBody(req events.APIGatewayV2HTTPRequest) string {
	if !req.IsBase64Encoded {
		return req.Body
	}
	decoded, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// header does a case-insensitive lookup; API Gateway v2 lowercases header
// names but tests and other event sources may not.
func header(req events.APIGatewayV2HTTPRequest, name string) string {
	for k, v := range req.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func jsonResp(status int, payload any) events.APIGatewayV2HTTPResponse {
	b, _ := json.Marshal(payload)
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}
}

func googleVerifier(clientID string) verifyGoogleFunc {
	return func(ctx context.Context, credential string) (string, string, error) {
		payload, err := idtoken.Validate(ctx, credential, clientID)
		if err != nil {
			return "", "", err
		}
		if verified, _ := payload.Claims["email_verified"].(bool); !verified {
			return "", "", fmt.Errorf("email not verified")
		}
		email, _ := payload.Claims["email"].(string)
		if payload.Subject == "" || email == "" {
			return "", "", fmt.Errorf("missing sub or email claim")
		}
		return payload.Subject, email, nil
	}
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s not set", name)
	}
	return v
}

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	table := mustEnv("TABLE_NAME")
	clientID := mustEnv("GOOGLE_CLIENT_ID")
	staticUser := os.Getenv("STATIC_TOKEN_USER")
	if staticUser == "" {
		staticUser = "joshua"
	}
	out, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(mustEnv("TOKEN_PARAM")),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatalf("read token param: %v", err)
	}
	h := &handler{
		store:        store.New(dynamodb.NewFromConfig(cfg), table),
		staticToken:  aws.ToString(out.Parameter.Value),
		staticUser:   staticUser,
		verifyGoogle: googleVerifier(clientID),
		html:         strings.ReplaceAll(indexHTML, "__GOOGLE_CLIENT_ID__", clientID),
		sessions:     map[string]*store.Session{},
	}
	lambda.Start(h.Handle)
}
