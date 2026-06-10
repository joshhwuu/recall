package store

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

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
