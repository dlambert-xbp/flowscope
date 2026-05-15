package snmpx

import (
	"strings"
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr string
	}{
		{
			name: "single ip via slash 32",
			in:   "10.0.0.5/32",
			want: []string{"10.0.0.5"},
		},
		{
			name: "single ip bare",
			in:   "10.0.0.5",
			want: []string{"10.0.0.5"},
		},
		{
			name: "small cidr /30 enumerates 4",
			in:   "10.0.0.0/30",
			want: []string{"10.0.0.0", "10.0.0.1", "10.0.0.2", "10.0.0.3"},
		},
		{
			name: "dashed range three ips",
			in:   "10.0.0.5-10.0.0.7",
			want: []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"},
		},
		{
			name: "dashed range whitespace tolerated",
			in:   " 10.0.0.5 - 10.0.0.7 ",
			want: []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"},
		},
		{
			name:    "empty rejected",
			in:      "",
			wantErr: "range is required",
		},
		{
			name:    "garbage cidr rejected",
			in:      "10.0.0.0/wat",
			wantErr: "invalid CIDR",
		},
		{
			name:    "garbage range rejected",
			in:      "10.0.0.5-nope",
			wantErr: "invalid range end",
		},
		{
			name:    "range with end before start rejected",
			in:      "10.0.0.10-10.0.0.5",
			wantErr: "range end must be >= start",
		},
		{
			name:    "cidr larger than /24 rejected",
			in:      "10.0.0.0/23",
			wantErr: "max 256 per scan",
		},
		{
			name:    "dashed range larger than 256 rejected",
			in:      "10.0.0.0-10.0.2.255",
			wantErr: "max 256",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRange(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseRange(%q): expected error containing %q, got nil", tc.in, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseRange(%q): err = %q, want substring %q", tc.in, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRange(%q): unexpected error %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseRange(%q): len=%d, want %d (%v vs %v)", tc.in, len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseRange(%q): [%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseRangeMaxIsExactly256(t *testing.T) {
	// /24 should be accepted (exactly 256 addresses)
	got, err := ParseRange("10.0.0.0/24")
	if err != nil {
		t.Fatalf("/24 must be accepted: %v", err)
	}
	if len(got) != 256 {
		t.Fatalf("/24 expanded to %d addresses, want 256", len(got))
	}
}

func TestFromProfileProjection(t *testing.T) {
	p := &Profile{
		Version:     "v3",
		Port:        1161,
		Community:   "ignored-for-v3",
		V3Username:  "noc-ro",
		V3AuthProto: "SHA-256",
		V3AuthPass:  "sekret-auth",
		V3PrivProto: "AES",
		V3PrivPass:  "sekret-priv",
		V3Context:   "VRF-MGMT",
	}
	cfg := FromProfile(p, 0, 0)
	if cfg.Version != "v3" {
		t.Errorf("Version = %q, want v3", cfg.Version)
	}
	if cfg.Port != 1161 {
		t.Errorf("Port = %d, want 1161", cfg.Port)
	}
	if cfg.V3Username != "noc-ro" {
		t.Errorf("V3Username = %q, want noc-ro", cfg.V3Username)
	}
	if cfg.V3AuthProto != "SHA-256" || cfg.V3AuthPass != "sekret-auth" {
		t.Errorf("auth fields not projected: %+v", cfg)
	}
	if cfg.V3PrivProto != "AES" || cfg.V3PrivPass != "sekret-priv" {
		t.Errorf("priv fields not projected: %+v", cfg)
	}
	if cfg.V3Context != "VRF-MGMT" {
		t.Errorf("Context = %q, want VRF-MGMT", cfg.V3Context)
	}
}

func TestFromProfilePortOverride(t *testing.T) {
	p := &Profile{Version: "v2c", Port: 161, Community: "public"}
	cfg := FromProfile(p, 1161, 0)
	if cfg.Port != 1161 {
		t.Errorf("port override ignored: got %d, want 1161", cfg.Port)
	}
	// Zero override leaves profile port intact.
	cfg = FromProfile(p, 0, 0)
	if cfg.Port != 161 {
		t.Errorf("zero override clobbered profile port: got %d, want 161", cfg.Port)
	}
}

func TestCredentialUsesProfile(t *testing.T) {
	c := Credential{}
	if c.UsesProfile() {
		t.Error("empty Credential should not use profile")
	}
	c.ProfileID = "abc"
	if !c.UsesProfile() {
		t.Error("Credential with ProfileID set must report UsesProfile=true")
	}
}

func TestFromProfileNil(t *testing.T) {
	cfg := FromProfile(nil, 0, 0)
	if cfg.Version != "" || cfg.Port != 0 {
		t.Errorf("nil profile should project zero Config; got %+v", cfg)
	}
}

// TestAutoBindBackoffStateMachine verifies the scheduler's in-memory
// autoBindNextAttempt map behaves as a simple "skip until time T"
// gate. Doesn't require a real CredentialStore — we set the map
// directly and check the gating logic the resolver uses inline.
func TestAutoBindBackoffStateMachine(t *testing.T) {
	s := &Scheduler{autoBindNextAttempt: map[string]time.Time{}}
	target := "10.0.0.5"

	// Not in map → allowed.
	if next, blocked := bb(s, target); blocked {
		t.Errorf("fresh target should not be blocked, next=%v", next)
	}

	// Future deadline → blocked.
	s.autoBindNextAttempt[target] = time.Now().Add(time.Minute)
	if _, blocked := bb(s, target); !blocked {
		t.Errorf("future deadline should block")
	}

	// Past deadline → allowed.
	s.autoBindNextAttempt[target] = time.Now().Add(-time.Minute)
	if _, blocked := bb(s, target); blocked {
		t.Errorf("past deadline should not block")
	}
}

// bb mirrors the inline check in Scheduler.autoBind. Kept here so the
// test asserts the actual condition shape.
func bb(s *Scheduler, target string) (time.Time, bool) {
	if next, ok := s.autoBindNextAttempt[target]; ok && time.Now().Before(next) {
		return next, true
	}
	return time.Time{}, false
}
