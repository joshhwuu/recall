package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const idemTTL = 24 * time.Hour

// Store wraps Dynamo access to the recall-main table.
type Store struct {
	client *dynamodb.Client
	table  string
}

func New(client *dynamodb.Client, table string) *Store {
	return &Store{client: client, table: table}
}

// PutNote writes a note item. The note's Key must already be set
// (use NoteKey).
func (s *Store) PutNote(ctx context.Context, n *Note) error {
	item, err := attributevalue.MarshalMap(n)
	if err != nil {
		return fmt.Errorf("marshal note %s: %w", n.ID, err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put note %s: %w", n.ID, err)
	}
	return nil
}

// PutNoteIdempotent writes a note plus an idempotency marker in one
// transaction. If the marker for idemKey already exists (a client retry),
// nothing is written and the originally created note's id is returned;
// existingID is empty when the note was written fresh.
func (s *Store) PutNoteIdempotent(ctx context.Context, n *Note, idemKey string) (existingID string, err error) {
	noteItem, err := attributevalue.MarshalMap(n)
	if err != nil {
		return "", fmt.Errorf("marshal note %s: %w", n.ID, err)
	}
	idem := IdemKey(idemKey)
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String(s.table),
				Item: map[string]types.AttributeValue{
					"PK":      &types.AttributeValueMemberS{Value: idem.PK},
					"SK":      &types.AttributeValueMemberS{Value: idem.SK},
					"note_id": &types.AttributeValueMemberS{Value: n.ID},
					"ttl":     &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(idemTTL).Unix(), 10)},
				},
				ConditionExpression: aws.String("attribute_not_exists(PK)"),
			}},
			{Put: &types.Put{
				TableName: aws.String(s.table),
				Item:      noteItem,
			}},
		},
	})
	if err == nil {
		return "", nil
	}

	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return "", fmt.Errorf("put note %s (idempotent): %w", n.ID, err)
	}
	replay := false
	for _, r := range canceled.CancellationReasons {
		if r.Code != nil && *r.Code == "ConditionalCheckFailed" {
			replay = true
		}
	}
	if !replay {
		return "", fmt.Errorf("put note %s (idempotent): %w", n.ID, err)
	}

	key, err := attributevalue.MarshalMap(idem)
	if err != nil {
		return "", fmt.Errorf("marshal idem key: %w", err)
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key,
	})
	if err != nil {
		return "", fmt.Errorf("get idem marker %s: %w", idemKey, err)
	}
	if out.Item == nil {
		// Marker condition failed but the item is gone (TTL race); treat
		// as a hard error rather than double-writing.
		return "", fmt.Errorf("idem marker %s vanished after conditional failure", idemKey)
	}
	var marker struct {
		NoteID string `dynamodbav:"note_id"`
	}
	if err := attributevalue.UnmarshalMap(out.Item, &marker); err != nil {
		return "", fmt.Errorf("unmarshal idem marker %s: %w", idemKey, err)
	}
	return marker.NoteID, nil
}

// GetNote fetches a note by user and ULID. Returns (nil, nil) if absent.
func (s *Store) GetNote(ctx context.Context, userID, noteULID string) (*Note, error) {
	key, err := attributevalue.MarshalMap(NoteKey(userID, noteULID))
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("get note %s: %w", noteULID, err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var n Note
	if err := attributevalue.UnmarshalMap(out.Item, &n); err != nil {
		return nil, fmt.Errorf("unmarshal note %s: %w", noteULID, err)
	}
	return &n, nil
}

// Session is a signed-in user's server-side session record.
type Session struct {
	UserID    string `dynamodbav:"user_id"` // Google sub claim
	Email     string `dynamodbav:"email"`
	ExpiresAt int64  `dynamodbav:"ttl"` // epoch seconds; doubles as Dynamo TTL
}

// CreateSession stores a session under the hashed token.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, sess Session) error {
	item, err := attributevalue.MarshalMap(sess)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	key := SessionKey(tokenHash)
	item["PK"] = &types.AttributeValueMemberS{Value: key.PK}
	item["SK"] = &types.AttributeValueMemberS{Value: key.SK}
	item["created_at"] = &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put session: %w", err)
	}
	return nil
}

// GetSession returns the live session for a hashed token, or nil if it
// doesn't exist or has expired. Expiry is checked here because Dynamo TTL
// deletion can lag up to ~48h behind the ttl timestamp.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	key, err := attributevalue.MarshalMap(SessionKey(tokenHash))
	if err != nil {
		return nil, fmt.Errorf("marshal session key: %w", err)
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var sess Session
	if err := attributevalue.UnmarshalMap(out.Item, &sess); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	if sess.ExpiresAt <= time.Now().Unix() {
		return nil, nil
	}
	return &sess, nil
}

// DeleteNote removes a note item.
func (s *Store) DeleteNote(ctx context.Context, userID, noteULID string) error {
	key, err := attributevalue.MarshalMap(NoteKey(userID, noteULID))
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	_, err = s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       key,
	})
	if err != nil {
		return fmt.Errorf("delete note %s: %w", noteULID, err)
	}
	return nil
}
