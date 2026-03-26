package worker

import (
	"sync"
	"time"
)

// LogEntry is a single log line with timestamp.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Raw     string    `json:"raw"`
}

// LogBuffer is a thread-safe ring buffer that captures log lines.
type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	size    int
	pos     int
	full    bool
}

// NewLogBuffer creates a ring buffer with the given capacity.
func NewLogBuffer(size int) *LogBuffer {
	return &LogBuffer{
		entries: make([]LogEntry, size),
		size:    size,
	}
}

// Write implements io.Writer. Each call is one log line.
func (lb *LogBuffer) Write(p []byte) (n int, err error) {
	line := string(p)
	entry := LogEntry{
		Time: time.Now(),
		Raw:  line,
	}
	// Parse level from zerolog console output (e.g. "6:20PM INF ...")
	entry.Level, entry.Message = parseLogLine(line)

	lb.mu.Lock()
	lb.entries[lb.pos] = entry
	lb.pos = (lb.pos + 1) % lb.size
	if lb.pos == 0 {
		lb.full = true
	}
	lb.mu.Unlock()

	return len(p), nil
}

// Entries returns all buffered entries in chronological order.
func (lb *LogBuffer) Entries() []LogEntry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if !lb.full {
		result := make([]LogEntry, lb.pos)
		copy(result, lb.entries[:lb.pos])
		return result
	}

	result := make([]LogEntry, lb.size)
	copy(result, lb.entries[lb.pos:])
	copy(result[lb.size-lb.pos:], lb.entries[:lb.pos])
	return result
}

// parseLogLine extracts level and message from zerolog console output.
func parseLogLine(line string) (level, message string) {
	// zerolog console format: "6:20PM INF Worker started port=34567"
	// or with color codes: "\x1b[90m6:20PM\x1b[0m \x1b[32mINF\x1b[0m ..."
	level = "info"
	message = line

	for _, lvl := range []struct {
		tag   string
		level string
	}{
		{"TRC", "trace"},
		{"DBG", "debug"},
		{"INF", "info"},
		{"WRN", "warn"},
		{"ERR", "error"},
		{"FTL", "fatal"},
	} {
		for i := 0; i < len(line)-len(lvl.tag); i++ {
			if line[i:i+len(lvl.tag)] == lvl.tag {
				level = lvl.level
				// Message starts after the level tag + space + bold marker
				rest := line[i+len(lvl.tag):]
				// Strip leading whitespace and ANSI codes
				j := 0
				for j < len(rest) && (rest[j] == ' ' || rest[j] == '\x1b') {
					if rest[j] == '\x1b' {
						for j < len(rest) && rest[j] != 'm' {
							j++
						}
						j++ // skip 'm'
					} else {
						j++
					}
				}
				if j < len(rest) {
					message = rest[j:]
				}
				return
			}
		}
	}
	return
}
