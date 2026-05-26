package resp

import "testing"

func TestEncodeBulkString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "$5\r\nhello\r\n"},
		{"", "$0\r\n\r\n"},
		{"a b c", "$5\r\na b c\r\n"},
	}
	for _, tt := range tests {
		got := string(EncodeBulkString(tt.input))
		if got != tt.want {
			t.Errorf("EncodeBulkString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEncodeInteger(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, ":0\r\n"},
		{42, ":42\r\n"},
		{-1, ":-1\r\n"},
	}
	for _, tt := range tests {
		got := string(EncodeInteger(tt.input))
		if got != tt.want {
			t.Errorf("EncodeInteger(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEncodeSimpleString(t *testing.T) {
	got := string(EncodeSimpleString("OK"))
	if got != "+OK\r\n" {
		t.Errorf("EncodeSimpleString(OK) = %q, want %q", got, "+OK\r\n")
	}
}

func TestEncodeError(t *testing.T) {
	got := string(EncodeError("ERR bad"))
	if got != "-ERR bad\r\n" {
		t.Errorf("EncodeError = %q, want %q", got, "-ERR bad\r\n")
	}
}

func TestEncodeArray(t *testing.T) {
	got := string(EncodeArray(
		EncodeBulkString("message"),
		EncodeBulkString("ch1"),
		EncodeBulkString("hello"),
	))
	want := "*3\r\n$7\r\nmessage\r\n$3\r\nch1\r\n$5\r\nhello\r\n"
	if got != want {
		t.Errorf("EncodeArray = %q, want %q", got, want)
	}
}

func TestAppendEquivalence(t *testing.T) {
	s := "test"
	alloc := EncodeBulkString(s)
	appended := AppendBulkString(nil, s)
	if string(alloc) != string(appended) {
		t.Errorf("Append/Encode mismatch: %q vs %q", appended, alloc)
	}

	allocInt := EncodeInteger(99)
	appendedInt := AppendInteger(nil, 99)
	if string(allocInt) != string(appendedInt) {
		t.Errorf("AppendInteger mismatch: %q vs %q", appendedInt, allocInt)
	}
}

func TestEncodeNulls(t *testing.T) {
	if got := string(EncodeNullBulk()); got != "$-1\r\n" {
		t.Errorf("EncodeNullBulk = %q", got)
	}
	if got := string(EncodeNullArray()); got != "*-1\r\n" {
		t.Errorf("EncodeNullArray = %q", got)
	}
}

func BenchmarkEncodeBulkString(b *testing.B) {
	b.Run("alloc", func(b *testing.B) {
		for b.Loop() {
			_ = EncodeBulkString("hello world")
		}
	})
	b.Run("append", func(b *testing.B) {
		buf := make([]byte, 0, 64)
		for b.Loop() {
			buf = AppendBulkString(buf[:0], "hello world")
		}
	})
}

func BenchmarkEncodeMessage(b *testing.B) {
	b.Run("alloc", func(b *testing.B) {
		for b.Loop() {
			_ = EncodeArray(
				EncodeBulkString("message"),
				EncodeBulkString("mychannel"),
				EncodeBulkString("hello world payload"),
			)
		}
	})
	b.Run("append", func(b *testing.B) {
		buf := make([]byte, 0, 128)
		for b.Loop() {
			buf = buf[:0]
			buf = AppendArrayHeader(buf, 3)
			buf = AppendBulkString(buf, "message")
			buf = AppendBulkString(buf, "mychannel")
			buf = AppendBulkString(buf, "hello world payload")
		}
	})
}
