package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type event struct{ Action, Package, Test, Output string }
type state struct {
	mentions bool
	skipped  bool
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	seen := false
	states := map[string]*state{}
	for scanner.Scan() {
		seen = true
		line := scanner.Bytes()
		fmt.Println(string(line))
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		key := e.Package + "\x00" + e.Test
		s := states[key]
		if s == nil {
			s = &state{}
			states[key] = s
		}
		if e.Action == "output" && (strings.Contains(e.Output, "DATABASE_URL") || strings.Contains(e.Output, "AGENT_MEM_TEST_DATABASE_URL")) {
			s.mentions = true
		}
		if e.Action == "skip" {
			s.skipped = true
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !seen {
		fmt.Fprintln(os.Stderr, "no go test JSON input")
		os.Exit(1)
	}
	failed := false
	for key, s := range states {
		if s.mentions && s.skipped {
			fmt.Fprintf(os.Stderr, "database test skipped: %s\n", strings.ReplaceAll(key, "\x00", " "))
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
