package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantOut  string
		wantCode int
	}{
		{"version", []string{"version"}, "narcissus devel (SQLite 3.", 0},
		{"--version", []string{"--version"}, "narcissus devel", 0},
		{"help", []string{"help"}, "Usage:", 0},
		{"no command", nil, "", 2},
		{"unknown command", []string{"sync"}, "", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(t.Context(), tt.args, &out, io.Discard)
			if code := exitCode(err, io.Discard); code != tt.wantCode {
				t.Errorf("exit code %d, want %d (error %v)", code, tt.wantCode, err)
			}
			if !strings.HasPrefix(out.String(), tt.wantOut) {
				t.Errorf("output %q, want prefix %q", out.String(), tt.wantOut)
			}
		})
	}
}
