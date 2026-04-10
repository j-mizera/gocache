package rex

import (
	"fmt"
	"strings"
)

// ParseMeta extracts a key and value from META command arguments.
// The first argument is the key, remaining arguments are joined with spaces
// to form the value. This allows values like "Bearer eyJhbG..." to be passed
// as separate RESP bulk strings or inline tokens.
//
// Examples:
//
//	["traceparent", "00-abc123"] → ("traceparent", "00-abc123", nil)
//	["authorization", "Bearer", "eyJhbG..."] → ("authorization", "Bearer eyJhbG...", nil)
func ParseMeta(args []string) (string, string, error) {
	if len(args) < 2 {
		return "", "", fmt.Errorf("META requires at least key and value")
	}
	key := args[0]
	if err := ValidateKey(key); err != nil {
		return "", "", err
	}
	value := strings.Join(args[1:], " ")
	return key, value, nil
}
