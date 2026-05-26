package resp

import (
	"bytes"
	"testing"
)

func TestRoundTrip_Null(t *testing.T) {
	input := "_\r\n"
	r := NewReader(bytes.NewBufferString(input))
	v, err := r.Read()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if v.Type != Null || !v.IsNull {
		t.Errorf("expected Null type with IsNull=true, got Type=%c IsNull=%v", v.Type, v.IsNull)
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(v); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	if buf.String() != input {
		t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), input)
	}
}

func TestRoundTrip_Boolean(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"true", "#t\r\n", true},
		{"false", "#f\r\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(bytes.NewBufferString(tt.input))
			v, err := r.Read()
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if v.Type != Boolean || v.Bool != tt.want {
				t.Errorf("expected Boolean=%v, got Type=%c Bool=%v", tt.want, v.Type, v.Bool)
			}

			var buf bytes.Buffer
			w := NewWriter(&buf)
			if err := w.Write(v); err != nil {
				t.Fatalf("write error: %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("flush error: %v", err)
			}
			if buf.String() != tt.input {
				t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), tt.input)
			}
		})
	}
}

func TestRoundTrip_Double(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   float64
		output string
	}{
		{"integer-like", ",3.14\r\n", 3.14, ",3.14\r\n"},
		{"negative", ",-1.5\r\n", -1.5, ",-1.5\r\n"},
		{"zero", ",0\r\n", 0, ",0\r\n"},
		{"large", ",1e10\r\n", 1e10, ",1e+10\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(bytes.NewBufferString(tt.input))
			v, err := r.Read()
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if v.Type != Double || v.Float64 != tt.want {
				t.Errorf("expected Double=%v, got Type=%c Float64=%v", tt.want, v.Type, v.Float64)
			}

			var buf bytes.Buffer
			w := NewWriter(&buf)
			if err := w.Write(v); err != nil {
				t.Fatalf("write error: %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("flush error: %v", err)
			}
			if buf.String() != tt.output {
				t.Errorf("serialize mismatch: got %q, want %q", buf.String(), tt.output)
			}
		})
	}
}

func TestRoundTrip_BulkError(t *testing.T) {
	input := "!11\r\nERR unknown\r\n"
	r := NewReader(bytes.NewBufferString(input))
	v, err := r.Read()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if v.Type != BulkError || v.Str != "ERR unknown" {
		t.Errorf("expected BulkError 'ERR unknown', got Type=%c Str=%q", v.Type, v.Str)
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(v); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	if buf.String() != input {
		t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), input)
	}
}

func TestRoundTrip_Map(t *testing.T) {
	// %2\r\n$3\r\nfoo\r\n$3\r\nbar\r\n$3\r\nbaz\r\n:42\r\n
	input := "%2\r\n$3\r\nfoo\r\n$3\r\nbar\r\n$3\r\nbaz\r\n:42\r\n"
	r := NewReader(bytes.NewBufferString(input))
	v, err := r.Read()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if v.Type != Map {
		t.Fatalf("expected Map type, got %c", v.Type)
	}
	if len(v.Array) != 4 {
		t.Fatalf("expected 4 elements (2 pairs), got %d", len(v.Array))
	}
	if v.Array[0].Str != "foo" || v.Array[1].Str != "bar" {
		t.Errorf("pair 1: got %q=%q, want foo=bar", v.Array[0].Str, v.Array[1].Str)
	}
	if v.Array[2].Str != "baz" || v.Array[3].Integer != 42 {
		t.Errorf("pair 2: got %q=%d, want baz=42", v.Array[2].Str, v.Array[3].Integer)
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(v); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	if buf.String() != input {
		t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), input)
	}
}

func TestRoundTrip_Set(t *testing.T) {
	input := "~3\r\n$3\r\naaa\r\n$3\r\nbbb\r\n$3\r\nccc\r\n"
	r := NewReader(bytes.NewBufferString(input))
	v, err := r.Read()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if v.Type != Set {
		t.Fatalf("expected Set type, got %c", v.Type)
	}
	if len(v.Array) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(v.Array))
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(v); err != nil {
		t.Fatalf("write error: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	if buf.String() != input {
		t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), input)
	}
}

func TestResp2BackwardCompat(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"SimpleString", "+OK\r\n"},
		{"Error", "-ERR something\r\n"},
		{"Integer", ":42\r\n"},
		{"BulkString", "$5\r\nhello\r\n"},
		{"Array", "*2\r\n$3\r\nfoo\r\n$3\r\nbar\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(bytes.NewBufferString(tt.input))
			v, err := r.Read()
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}

			var buf bytes.Buffer
			w := NewWriter(&buf)
			if err := w.Write(v); err != nil {
				t.Fatalf("write error: %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("flush error: %v", err)
			}
			if buf.String() != tt.input {
				t.Errorf("round-trip mismatch: got %q, want %q", buf.String(), tt.input)
			}
		})
	}
}

func TestHelperConstructors(t *testing.T) {
	n := NullV3()
	if n.Type != Null || !n.IsNull {
		t.Error("NullV3 wrong")
	}

	tr := True()
	if tr.Type != Boolean || !tr.Bool {
		t.Error("True wrong")
	}

	fa := False()
	if fa.Type != Boolean || fa.Bool {
		t.Error("False wrong")
	}

	d := MarshalDouble(3.14)
	if d.Type != Double || d.Float64 != 3.14 {
		t.Error("MarshalDouble wrong")
	}

	be := MarshalBulkError("ERR test")
	if be.Type != BulkError || be.Str != "ERR test" {
		t.Error("MarshalBulkError wrong")
	}

	m := MapFromPairs(MarshalBulkString("k"), MarshalBulkString("v"))
	if m.Type != Map || len(m.Array) != 2 {
		t.Error("MapFromPairs wrong")
	}

	s := SetFromValues(MarshalBulkString("a"), MarshalBulkString("b"))
	if s.Type != Set || len(s.Array) != 2 {
		t.Error("SetFromValues wrong")
	}
}
