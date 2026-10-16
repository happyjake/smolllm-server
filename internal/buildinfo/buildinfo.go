// Package buildinfo exposes the identity of the running executable, not a file
// that may have been replaced on disk. Release builds stamp both repositories.
package buildinfo

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"runtime"
)

// Set only by launch/build.py through -ldflags. Plain go build remains usable,
// but does not claim verified provenance for the local replacement dependency.
var (
	serverRevision  = "unknown"
	libraryRevision = "unknown"
	sourceClean     = "false"
	instanceID      = newInstanceID()
	revisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

type Info struct {
	SchemaVersion   int    `json:"schema_version"`
	ServerRevision  string `json:"server_revision"`
	LibraryRevision string `json:"library_revision"`
	SourceClean     bool   `json:"source_clean"`
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
	GoVersion       string `json:"go_version"`
	InstanceID      string `json:"instance_id"`
	PID             int    `json:"pid"`
}

func Current() Info {
	return Info{
		SchemaVersion: 1, ServerRevision: serverRevision, LibraryRevision: libraryRevision,
		SourceClean: sourceClean == "true" && revisionPattern.MatchString(serverRevision) && revisionPattern.MatchString(libraryRevision),
		GOOS:        runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(),
		InstanceID: instanceID, PID: os.Getpid(),
	}
}

func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cannot create process identity")
	}
	return hex.EncodeToString(b[:])
}
