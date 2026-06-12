// Command ingest is the capture-path Lambda behind the recall HTTP API.
// POST /entries writes a raw, unenriched note to Dynamo; GET / serves the
// embedded capture page. No AI runs in this path (see PLAN.md, Phase 2).
package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/oklog/ulid/v2"

	"github.com/joshhwuu/recall/pkg/store"
)

//go:embed index.html
var indexHTML string

const (
	userID     = "joshua"
	maxTextLen = 10 * 1024
)

type noteStore interface {
	PutNote(ctx context.Context, n *store.Note) error
	PutNoteIdempotent(ctx context.Context, n *store.Note, idemKey string) (existingID string, err error)
}

type handler struct {
	store noteStore
	token string
}

type entryRequest struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

func (h *handler) Handle(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	method := req.RequestContext.HTTP.Method
	path := req.RawPath
	switch {
	case method == "GET" && (path == "/" || path == ""):
		return events.APIGatewayV2HTTPResponse{
			StatusCode: 200,
			Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8"},
			Body:       indexHTML,
		}, nil
	case method == "POST" && path == "/entries":
		return h.postEntry(ctx, req), nil
	default:
		return jsonResp(404, map[string]string{"error": "not found"}), nil
	}
}

func (h *handler) postEntry(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	auth := header(req, "authorization")
	bearer, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(bearer), []byte(h.token)) != 1 {
		return jsonResp(401, map[string]string{"error": "unauthorized"})
	}

	rawBody := req.Body
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(rawBody)
		if err != nil {
			return jsonResp(400, map[string]string{"error": "invalid body encoding"})
		}
		rawBody = string(decoded)
	}
	var body entryRequest
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
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

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	table := os.Getenv("TABLE_NAME")
	if table == "" {
		log.Fatal("TABLE_NAME not set")
	}
	param := os.Getenv("TOKEN_PARAM")
	if param == "" {
		log.Fatal("TOKEN_PARAM not set")
	}
	out, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(param),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatalf("read token param %s: %v", param, err)
	}
	h := &handler{
		store: store.New(dynamodb.NewFromConfig(cfg), table),
		token: aws.ToString(out.Parameter.Value),
	}
	lambda.Start(h.Handle)
}
