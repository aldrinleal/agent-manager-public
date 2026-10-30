package util

import (
	"os"
	"strconv"
	"time"
)

// EnvIf looks up the first non-empty environment variable from the arguments,
// or returns the last argument if all are empty. Lifted verbatim from
// ../api-kit/util/envif.go so the two behave identically.
func EnvIf(args ...string) string {
	lastIndex := -1 + len(args)

	result := args[lastIndex]

	for _, envVarName := range args[0:lastIndex] {
		if newValue, ok := os.LookupEnv(envVarName); ok && newValue != "" {
			return newValue
		}
	}

	return result
}

// EnvInt reads an integer environment variable, falling back to def when unset
// or unparseable.
func EnvInt(name string, def int) int {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return def
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}

	return v
}

// EnvBool reads a boolean environment variable. Accepts everything
// strconv.ParseBool does ("1", "t", "true", "TRUE", "0", "f", "false", ...).
func EnvBool(name string, def bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return def
	}

	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}

	return v
}

// EnvSeconds reads an integer environment variable and returns it as a
// duration in seconds.
func EnvSeconds(name string, def int) time.Duration {
	return time.Duration(EnvInt(name, def)) * time.Second
}
