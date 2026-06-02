package main

import (
	"slices"
	"testing"
)

func TestMergeTags(t *testing.T) {
	tests := []struct {
		name string
		have []string
		add  []string
		want []string
	}{
		{"create (no existing)", nil, []string{"vscode"}, []string{"vscode"}},
		{"append new", []string{"go"}, []string{"vscode"}, []string{"go", "vscode"}},
		{"dedupe overlap", []string{"go", "vscode"}, []string{"vscode", "work"}, []string{"go", "vscode", "work"}},
		{"no new tags", []string{"go"}, nil, []string{"go"}},
		{"dedupe within add", nil, []string{"a", "a", "b"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeTags(tt.have, tt.add); !slices.Equal(got, tt.want) {
				t.Errorf("mergeTags(%v, %v) = %v, want %v", tt.have, tt.add, got, tt.want)
			}
		})
	}
}
