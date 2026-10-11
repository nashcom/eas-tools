package exporter

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// A Source is where the configuration is read from again and again while the exporter runs: a file or an https://
// location. The caller decides what a fetched document means; Accept tells the source that the document was valid, so
// that the next Fetch may answer ErrNotModified for it. A document that was refused is not accepted, so it is fetched
// again and keeps being reported until it is fixed.
type Source interface {
	Fetch(ctx context.Context) ([]byte, error)
	Accept()
}

// FileSource is a configuration file that is checked by its modification time and size before it is read.
type FileSource struct {
	Path string

	mtime, pendingMtime time.Time
	size, pendingSize   int64
	known               bool
}

// NewFileSource returns a source for the file. Call Prime before the first read of the file, so that a change between
// that read and the first check is not missed.
func NewFileSource(path string) *FileSource { return &FileSource{Path: path} }

// Prime records the state of the file as the one that is in use.
func (f *FileSource) Prime() {
	if st, err := os.Stat(f.Path); err == nil {
		f.mtime, f.size, f.known = st.ModTime(), st.Size(), true
	}
}

// Fetch returns the file, or ErrNotModified when its time and size are those of the file that was accepted.
func (f *FileSource) Fetch(ctx context.Context) ([]byte, error) {
	st, err := os.Stat(f.Path)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration file: %w", err)
	}
	if f.known && st.ModTime().Equal(f.mtime) && st.Size() == f.size {
		return nil, ErrNotModified
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("reading the configuration file: %w", err)
	}
	if len(raw) > maxRemoteConfig {
		return nil, fmt.Errorf("the configuration file is bigger than %d bytes", maxRemoteConfig)
	}
	f.pendingMtime, f.pendingSize = st.ModTime(), st.Size()
	return raw, nil
}

// Accept records the file that the last Fetch returned as the one in use.
func (f *FileSource) Accept() {
	f.mtime, f.size, f.known = f.pendingMtime, f.pendingSize, true
}

// ConfigCheckSeconds returns how often the configuration is checked for a change: EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS, or
// def. 0 switches the check off.
func ConfigCheckSeconds(environ []string, def int) (int, error) {
	for _, line := range environ {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k != envConfigCheck || v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s=%q: not a number of seconds", envConfigCheck, v)
		}
		return n, nil
	}
	return def, nil
}

// Compared says what the next check compares with, for the log: the time and size of the file that is in use.
func (f *FileSource) Compared() string {
	if !f.known {
		return "nothing yet: the file is read at the first check"
	}
	return fmt.Sprintf("modification time %s, size %d bytes", f.mtime.UTC().Format(time.RFC3339), f.size)
}
