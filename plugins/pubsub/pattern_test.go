package main

import "testing"

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		want    bool
	}{
		{"*", "", true},
		{"*", "hello", true},
		{"*", "hello.world", true},
		{"h?llo", "hello", true},
		{"h?llo", "hallo", true},
		{"h?llo", "hllo", false},
		{"h[ae]llo", "hello", true},
		{"h[ae]llo", "hallo", true},
		{"h[ae]llo", "hillo", false},
		{"h[a-e]llo", "hcllo", true},
		{"h[a-e]llo", "hfllo", false},
		{"h[^e]llo", "hallo", true},
		{"h[^e]llo", "hello", false},
		{"hello", "hello", true},
		{"hello", "world", false},
		{"hello.*", "hello.world", true},
		{"hello.*", "hello.", true},
		{"hello.*", "hello", false},
		{"*.world", "hello.world", true},
		{"*.world", "world", false},
		{"news.*", "news.art.figurative", true},
		{"news.*", "news.", true},
		{"news.*", "news", false},
		{"\\*special", "*special", true},
		{"\\*special", "xspecial", false},
		{"\\?mark", "?mark", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"", "", true},
		{"", "a", false},
		{"a", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			got := matchPattern(tt.pattern, tt.str)
			if got != tt.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
			}
		})
	}
}
