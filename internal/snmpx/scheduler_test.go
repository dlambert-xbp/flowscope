package snmpx

import (
	"testing"
	"time"
)

func TestEffectiveInterval(t *testing.T) {
	s := &Scheduler{interval: 15 * time.Minute}

	tests := []struct {
		name      string
		exp       string
		overrides map[string]time.Duration
		want      time.Duration
	}{
		{
			name: "per-binding override wins",
			exp:  "10.0.0.1",
			overrides: map[string]time.Duration{
				"10.0.0.1": 60 * time.Second,
			},
			want: 60 * time.Second,
		},
		{
			name:      "no override falls back to cluster default",
			exp:       "10.0.0.2",
			overrides: map[string]time.Duration{},
			want:      15 * time.Minute,
		},
		{
			name:      "nil map falls back to cluster default",
			exp:       "10.0.0.3",
			overrides: nil,
			want:      15 * time.Minute,
		},
		{
			name: "zero override is ignored",
			exp:  "10.0.0.4",
			overrides: map[string]time.Duration{
				"10.0.0.4": 0,
			},
			want: 15 * time.Minute,
		},
		{
			name: "override on a different exporter does not bleed through",
			exp:  "10.0.0.5",
			overrides: map[string]time.Duration{
				"10.0.0.99": 30 * time.Second,
			},
			want: 15 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.effectiveInterval(tc.exp, tc.overrides)
			if got != tc.want {
				t.Fatalf("effectiveInterval(%q) = %v; want %v", tc.exp, got, tc.want)
			}
		})
	}
}
