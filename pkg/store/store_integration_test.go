//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ulidpkg "github.com/oklog/ulid/v2"
)

// Round-trips a dummy note against the live recall-main table.
// Requires the `recall` AWS profile (or ambient credentials) and a
// deployed table. Run with: go test -tags=integration ./pkg/store/
func TestPutGetNote_Live(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	table := os.Getenv("RECALL_TABLE")
	if table == "" {
		table = "recall-main"
	}
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "recall"
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	s := New(dynamodb.NewFromConfig(cfg), table)

	id := ulidpkg.Make().String()
	want := &Note{
		Key:       NoteKey("joshua", id),
		ID:        id,
		RawText:   "jaden bday march 12",
		Source:    "test",
		Enriched:  false,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.PutNote(ctx, want); err != nil {
		t.Fatalf("PutNote: %v", err)
	}
	t.Cleanup(func() {
		if err := s.DeleteNote(context.Background(), "joshua", id); err != nil {
			t.Errorf("cleanup DeleteNote: %v", err)
		}
	})

	got, err := s.GetNote(ctx, "joshua", id)
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if got == nil {
		t.Fatal("GetNote returned nil for a note that was just put")
	}
	if got.ID != want.ID || got.RawText != want.RawText ||
		got.Source != want.Source || got.Enriched != want.Enriched ||
		got.CreatedAt != want.CreatedAt {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}
