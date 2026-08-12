// Package envfile loads KEY=VALUE pairs from a local .env file into the
// process environment, for the docker-compose Postgres/Redis credentials
// (see README's "Current task" section). Deliberately no dependency
// (joho/godotenv or similar) -- the format needed here is tiny.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// Load reads path line by line and calls os.Setenv for each KEY=VALUE
// pair, skipping blank lines and lines starting with '#'. A real
// environment variable that's already set always wins -- Load never
// overwrites it -- so this is safe to call unconditionally in both local
// dev (.env present) and a real deployment (.env absent, real env vars
// set directly).
//
// A missing file at path is not an error: .env is optional by design.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		os.Setenv(key, value)
	}
	return scanner.Err()
}
