package testutil

import (
	"os"
)

func WriteFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
