package rex

import "testing"

func TestParseMeta(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantKey   string
		wantValue string
		wantErr   bool
	}{
		{
			name:      "simple key-value",
			args:      []string{"traceparent", "00-abc123"},
			wantKey:   "traceparent",
			wantValue: "00-abc123",
		},
		{
			name:      "value with spaces joined",
			args:      []string{"authorization", "Bearer", "eyJhbG..."},
			wantKey:   "authorization",
			wantValue: "Bearer eyJhbG...",
		},
		{
			name:      "dotted key",
			args:      []string{"auth.jwt", "token"},
			wantKey:   "auth.jwt",
			wantValue: "token",
		},
		{
			name:    "no args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "key only",
			args:    []string{"traceparent"},
			wantErr: true,
		},
		{
			name:    "reserved underscore key",
			args:    []string{"_internal", "val"},
			wantErr: true,
		},
		{
			name:    "reserved shared prefix",
			args:    []string{"shared.x", "val"},
			wantErr: true,
		},
		{
			name:    "empty key",
			args:    []string{"", "val"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := ParseMeta(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if key != tt.wantKey {
				t.Errorf("key=%q, want %q", key, tt.wantKey)
			}
			if value != tt.wantValue {
				t.Errorf("value=%q, want %q", value, tt.wantValue)
			}
		})
	}
}
