package main

// matchPattern implements Redis-compatible glob matching.
//
// Supported syntax:
//   - *      matches any sequence of characters (including empty)
//   - ?      matches exactly one character
//   - [abc]  matches one character in the set
//   - [a-z]  matches one character in the range
//   - [^a]   matches one character NOT in the set
//   - \x     matches literal x (escape)
func matchPattern(pattern, str string) bool {
	return match([]byte(pattern), []byte(str))
}

func match(pattern, str []byte) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			pattern = trimStars(pattern)
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(str); i++ {
				if match(pattern, str[i:]) {
					return true
				}
			}
			return false

		case '?':
			if len(str) == 0 {
				return false
			}
			pattern = pattern[1:]
			str = str[1:]

		case '[':
			if len(str) == 0 {
				return false
			}
			matched, rest, ok := matchCharClass(pattern, str[0])
			if !ok || !matched {
				return false
			}
			pattern = rest
			str = str[1:]

		case '\\':
			pattern = pattern[1:]
			if len(pattern) == 0 || len(str) == 0 || pattern[0] != str[0] {
				return false
			}
			pattern = pattern[1:]
			str = str[1:]

		default:
			if len(str) == 0 || pattern[0] != str[0] {
				return false
			}
			pattern = pattern[1:]
			str = str[1:]
		}
	}
	return len(str) == 0
}

func trimStars(pattern []byte) []byte {
	for len(pattern) > 0 && pattern[0] == '*' {
		pattern = pattern[1:]
	}
	return pattern
}

// matchCharClass parses a [...] class starting at pattern[0]=='[' and
// tests whether ch is in the class. Returns (matched, restOfPattern, valid).
func matchCharClass(pattern []byte, ch byte) (bool, []byte, bool) {
	pattern = pattern[1:] // skip '['
	negate := false
	if len(pattern) > 0 && pattern[0] == '^' {
		negate = true
		pattern = pattern[1:]
	}

	matched := false
	for len(pattern) > 0 && pattern[0] != ']' {
		lo := pattern[0]
		pattern = pattern[1:]

		if len(pattern) >= 2 && pattern[0] == '-' {
			hi := pattern[1]
			pattern = pattern[2:]
			if ch >= lo && ch <= hi {
				matched = true
			}
		} else if ch == lo {
			matched = true
		}
	}

	if len(pattern) == 0 {
		return false, nil, false
	}
	pattern = pattern[1:] // skip ']'

	if negate {
		matched = !matched
	}
	return matched, pattern, true
}
