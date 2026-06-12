package store

import "testing"

const ulid = "01J9ZW9GJ0EXAMPLE000000000"

func TestNoteKey(t *testing.T) {
	k := NoteKey("joshua", ulid)
	if k.PK != "USER#joshua" {
		t.Errorf("PK = %q, want USER#joshua", k.PK)
	}
	if k.SK != "NOTE#"+ulid {
		t.Errorf("SK = %q, want NOTE#%s", k.SK, ulid)
	}
}

func TestEntityEdgeKey(t *testing.T) {
	k := EntityEdgeKey("jaden", ulid)
	if k.PK != "ENTITY#jaden" || k.SK != "NOTE#"+ulid {
		t.Errorf("got %+v", k)
	}
}

func TestRecurringDateKey(t *testing.T) {
	k := RecurringDateKey(3, 12, ulid)
	if k.PK != "DATE#R#03-12" {
		t.Errorf("PK = %q, want DATE#R#03-12 (zero-padded)", k.PK)
	}
	if k.SK != "NOTE#"+ulid {
		t.Errorf("SK = %q", k.SK)
	}
}

func TestDateKey(t *testing.T) {
	k := DateKey("2026-03-12", ulid)
	if k.PK != "DATE#2026-03-12" || k.SK != "NOTE#"+ulid {
		t.Errorf("got %+v", k)
	}
}

func TestTypeEdgeKey(t *testing.T) {
	k := TypeEdgeKey("reminder", "2026-03-12T09:00:00Z", ulid)
	if k.PK != "TYPE#reminder" {
		t.Errorf("PK = %q", k.PK)
	}
	if k.SK != "2026-03-12T09:00:00Z#NOTE#"+ulid {
		t.Errorf("SK = %q", k.SK)
	}
}

func TestIdemKey(t *testing.T) {
	k := IdemKey("sms-abc123")
	if k.PK != "IDEM#sms-abc123" || k.SK != "IDEM" {
		t.Errorf("got %+v", k)
	}
}

func TestSessionKey(t *testing.T) {
	k := SessionKey("deadbeef")
	if k.PK != "SESSION#deadbeef" || k.SK != "SESSION" {
		t.Errorf("got %+v", k)
	}
}

func TestReminderGSIKeys(t *testing.T) {
	pk, sk := ReminderGSIKeys("joshua", "2026-03-12T09:00:00Z")
	if pk != "USER#joshua#REMINDER" {
		t.Errorf("GSI1PK = %q", pk)
	}
	if sk != "2026-03-12T09:00:00Z" {
		t.Errorf("GSI1SK = %q", sk)
	}
}
