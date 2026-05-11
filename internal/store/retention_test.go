package store

import (
	"strings"
	"testing"
)

func TestBuildAlterTTLStatement_Flows(t *testing.T) {
	got, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "flows", TSColumn: "observed"},
		30,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "ALTER TABLE flows MODIFY TTL toDateTime(observed) + INTERVAL 30 DAY"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAlterTTLStatement_Counters(t *testing.T) {
	got, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "iface_counter_samples", TSColumn: "ts"},
		90,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "ALTER TABLE iface_counter_samples MODIFY TTL toDateTime(ts) + INTERVAL 90 DAY"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAlterTTLStatement_RejectsZero(t *testing.T) {
	if _, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "flows", TSColumn: "observed"}, 0,
	); err == nil {
		t.Fatal("expected error on days <= 0")
	}
}

func TestBuildAlterTTLStatement_RejectsNegative(t *testing.T) {
	if _, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "flows", TSColumn: "observed"}, -1,
	); err == nil {
		t.Fatal("expected error on negative days")
	}
}

func TestBuildAlterTTLStatement_RejectsEmptyTable(t *testing.T) {
	if _, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "", TSColumn: "observed"}, 7,
	); err == nil {
		t.Fatal("expected error on empty table")
	}
}

func TestBuildAlterTTLStatement_RejectsEmptyColumn(t *testing.T) {
	if _, err := BuildAlterTTLStatement(
		RetentionTarget{Table: "flows", TSColumn: ""}, 7,
	); err == nil {
		t.Fatal("expected error on empty column")
	}
}

func TestParseDaysJSON_BareNumber(t *testing.T) {
	got, err := parseDaysJSON("30", 7)
	if err != nil || got != 30 {
		t.Fatalf("got (%d, %v), want (30, nil)", got, err)
	}
}

func TestParseDaysJSON_Quoted(t *testing.T) {
	got, _ := parseDaysJSON(`"45"`, 7)
	if got != 45 {
		t.Fatalf("got %d, want 45", got)
	}
}

func TestParseDaysJSON_Float(t *testing.T) {
	got, _ := parseDaysJSON("60.0", 7)
	if got != 60 {
		t.Fatalf("got %d, want 60", got)
	}
}

func TestParseDaysJSON_Empty(t *testing.T) {
	got, _ := parseDaysJSON("", 7)
	if got != 7 {
		t.Fatalf("got %d, want 7 (default)", got)
	}
}

func TestParseDaysJSON_Junk(t *testing.T) {
	got, _ := parseDaysJSON("not-a-number", 7)
	if got != 7 {
		t.Fatalf("got %d, want 7 (default fallback)", got)
	}
}

func TestParseDaysJSON_Zero(t *testing.T) {
	// Zero/negative is treated as junk and falls back to the default
	// — never want to ALTER ... INTERVAL 0 DAY which would TTL
	// everything immediately.
	got, _ := parseDaysJSON("0", 7)
	if got != 7 {
		t.Fatalf("got %d, want 7 (default, zero rejected)", got)
	}
}

func TestParseDaysJSON_Negative(t *testing.T) {
	got, _ := parseDaysJSON("-5", 7)
	if got != 7 {
		t.Fatalf("got %d, want 7 (default, negative rejected)", got)
	}
}

func TestTTLIntervalRegex_FreshMigration(t *testing.T) {
	// Form a fresh migration leaves in engine_full.
	in := "MergeTree PARTITION BY toYYYYMMDD(observed) " +
		"ORDER BY (toStartOfMinute(observed), exporter) " +
		"TTL toDateTime(observed) + INTERVAL 7 DAY " +
		"SETTINGS index_granularity = 8192"
	m := ttlIntervalRE.FindStringSubmatch(in)
	if m == nil {
		t.Fatal("regex did not match INTERVAL N DAY form")
	}
	if !strings.Contains(in, m[0]) {
		t.Errorf("match %q not in input", m[0])
	}
	// Group 2 (INTERVAL form) should be "7"; group 1 (toIntervalDay) empty.
	if m[1] != "" || m[2] != "7" {
		t.Errorf("got groups (%q, %q), want (\"\", \"7\")", m[1], m[2])
	}
}

func TestTTLIntervalRegex_PostAlter(t *testing.T) {
	// Form ClickHouse stores after MODIFY TTL — toIntervalDay(N).
	in := "MergeTree PARTITION BY toYYYYMMDD(ts) " +
		"ORDER BY (exporter, ifindex, ts) " +
		"TTL toDateTime(ts) + toIntervalDay(45) " +
		"SETTINGS index_granularity = 8192"
	m := ttlIntervalRE.FindStringSubmatch(in)
	if m == nil {
		t.Fatal("regex did not match toIntervalDay(N) form")
	}
	if m[1] != "45" {
		t.Errorf("got group 1 = %q, want \"45\"", m[1])
	}
}

func TestTTLIntervalRegex_NoMatch(t *testing.T) {
	in := "MergeTree ORDER BY (a) SETTINGS index_granularity = 8192"
	if m := ttlIntervalRE.FindStringSubmatch(in); m != nil {
		t.Errorf("expected no match, got %v", m)
	}
}
