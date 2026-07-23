package runner

import (
	"encoding/json"
	"fmt"
)

// EntrypointFile is the JSON file a build writes into its output root to declare
// how the artifact is run (command + args + env). `jobs-client run` reads it from
// the build output's c/ tree and executes it; jobs-registry reads it to fill the
// image config.
const EntrypointFile = "JOBS.entrypoint"

// entrypointFile is the historical unexported alias.
const entrypointFile = EntrypointFile

// DecodeEntrypoint parses and validates a JOBS.entrypoint JSON document.
func DecodeEntrypoint(b []byte) (Entrypoint, error) { return decodeEntrypoint(b) }

// Entrypoint is the decoded JOBS.entrypoint. Command is required; a path without
// a leading "/" is resolved against the output root, a leading "/" is used
// verbatim (an absolute path inside the run sandbox). Args and Env default empty.
type Entrypoint struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// decodeEntrypoint parses and validates a JOBS.entrypoint JSON document.
func decodeEntrypoint(b []byte) (Entrypoint, error) {
	var ep Entrypoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return Entrypoint{}, fmt.Errorf("invalid %s: %w", entrypointFile, err)
	}
	if ep.Command == "" {
		return Entrypoint{}, fmt.Errorf("invalid %s: command is required", entrypointFile)
	}
	if ep.Args == nil {
		ep.Args = []string{}
	}
	if ep.Env == nil {
		ep.Env = map[string]string{}
	}
	return ep, nil
}
