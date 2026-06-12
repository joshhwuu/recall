// Package store defines the recall-main single-table item model and Dynamo access.
//
// Key layout (see PLAN.md, Phase 1):
//
//	Note:           PK=USER#<user>        SK=NOTE#<ULID>
//	Entity edge:    PK=ENTITY#<slug>      SK=NOTE#<ULID>
//	Recurring date: PK=DATE#R#<MM-DD>     SK=NOTE#<ULID>
//	One-off date:   PK=DATE#<YYYY-MM-DD>  SK=NOTE#<ULID>
//	Type edge:      PK=TYPE#<type>        SK=<sortKey>#NOTE#<ULID>
//	Reminder GSI:   GSI1PK=USER#<user>#REMINDER  GSI1SK=<due ISO8601>
package store

import "fmt"

const (
	PrefixUser          = "USER#"
	PrefixNote          = "NOTE#"
	PrefixEntity        = "ENTITY#"
	PrefixDate          = "DATE#"
	PrefixRecurringDate = "DATE#R#"
	PrefixType          = "TYPE#"
	PrefixIdem          = "IDEM#"
	ReminderSuffix      = "#REMINDER"
)

// Key is a composite primary key in the recall-main table.
type Key struct {
	PK string `dynamodbav:"PK"`
	SK string `dynamodbav:"SK"`
}

// Entity is a person, place, or thing extracted from a note.
type Entity struct {
	Name string `dynamodbav:"name" json:"name"`
	Kind string `dynamodbav:"kind" json:"kind"` // person|place|thing
}

// When is the normalized time reference of a note: either a concrete
// one-off Date, or a recurring Month/Day (e.g. a birthday).
type When struct {
	Date       string `dynamodbav:"date,omitempty" json:"date,omitempty"` // YYYY-MM-DD
	Month      int    `dynamodbav:"month,omitempty" json:"month,omitempty"`
	Day        int    `dynamodbav:"day,omitempty" json:"day,omitempty"`
	Recurrence string `dynamodbav:"recurrence,omitempty" json:"recurrence,omitempty"` // e.g. "yearly"
}

// Note is the source-of-truth item. Raw fields are written at ingestion;
// the rest are filled in by the enrichment Lambda.
type Note struct {
	Key
	ID        string   `dynamodbav:"id"`
	RawText   string   `dynamodbav:"raw_text"`
	Source    string   `dynamodbav:"source"` // web|sms|shortcut
	Enriched  bool     `dynamodbav:"enriched"`
	CreatedAt string   `dynamodbav:"created_at"` // ISO8601

	Canonical string   `dynamodbav:"canonical,omitempty"`
	Type      string   `dynamodbav:"type,omitempty"` // event|reminder|fact|idea|journal
	Subtype   string   `dynamodbav:"subtype,omitempty"`
	Entities  []Entity `dynamodbav:"entities,omitempty"`
	When      *When    `dynamodbav:"when,omitempty"`
	Tags      []string `dynamodbav:"tags,omitempty"`
	Facts     []string `dynamodbav:"facts,omitempty"`
	// Embeddings holds one vector per fact: 384 float32, little-endian.
	Embeddings [][]byte `dynamodbav:"embeddings,omitempty"`

	// Reminder projection (sparse GSI1); set only when Type == "reminder".
	GSI1PK string `dynamodbav:"GSI1PK,omitempty"`
	GSI1SK string `dynamodbav:"GSI1SK,omitempty"`
}

// NoteKey returns the primary key of a note item.
func NoteKey(userID, noteULID string) Key {
	return Key{
		PK: PrefixUser + userID,
		SK: PrefixNote + noteULID,
	}
}

// EntityEdgeKey returns the key of an entity→note edge item.
func EntityEdgeKey(entitySlug, noteULID string) Key {
	return Key{
		PK: PrefixEntity + entitySlug,
		SK: PrefixNote + noteULID,
	}
}

// RecurringDateKey returns the key of a recurring-date edge (e.g. birthdays).
func RecurringDateKey(month, day int, noteULID string) Key {
	return Key{
		PK: fmt.Sprintf("%s%02d-%02d", PrefixRecurringDate, month, day),
		SK: PrefixNote + noteULID,
	}
}

// DateKey returns the key of a one-off date edge. date is YYYY-MM-DD.
func DateKey(date, noteULID string) Key {
	return Key{
		PK: PrefixDate + date,
		SK: PrefixNote + noteULID,
	}
}

// TypeEdgeKey returns the key of a type→note edge. sortKey orders notes
// within the type partition (e.g. a due timestamp or created_at).
func TypeEdgeKey(noteType, sortKey, noteULID string) Key {
	return Key{
		PK: PrefixType + noteType,
		SK: sortKey + "#" + PrefixNote + noteULID,
	}
}

// IdemKey returns the key of an idempotency marker item. Markers carry a
// note_id attribute and a 24h TTL; a conditional put on one makes note
// creation replay-safe (e.g. SMS webhook retries).
func IdemKey(clientKey string) Key {
	return Key{
		PK: PrefixIdem + clientKey,
		SK: "IDEM",
	}
}

// ReminderGSIKeys returns the sparse GSI1 attributes for a reminder note.
// dueISO is the reminder's due time in ISO8601 (UTC), which sorts lexically.
func ReminderGSIKeys(userID, dueISO string) (gsi1pk, gsi1sk string) {
	return PrefixUser + userID + ReminderSuffix, dueISO
}
