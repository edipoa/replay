package envutil

import (
	"fmt"
	"os"
	"strconv"
)

// MustEnv returns the value of key or exits with a descriptive error.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "variável de ambiente obrigatória não definida: %s\n", key)
		os.Exit(1)
	}
	return v
}

// Or returns the value of key, or fallback when the variable is unset/empty.
func Or(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// IntOr returns the integer value of key, or fallback on parse failure or missing variable.
func IntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
