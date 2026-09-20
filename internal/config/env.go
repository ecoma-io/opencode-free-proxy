package config

import "os"

// lookupEnv is the os.Getenv indirection the config loader uses so tests can
// pin the environment without global race.
func lookupEnv(name string) (string, bool) { return os.LookupEnv(name) }
