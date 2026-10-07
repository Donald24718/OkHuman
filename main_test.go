package main

import (
	"strings"
	"testing"
)

// TestPortConflictDetection 测试端口冲突错误消息的跨平台检测
func TestPortConflictDetection(t *testing.T) {
	tests := []struct {
		name     string
		errMsg   string
		wantHit  bool
	}{
		{
			name:    "Linux: address already in use",
			errMsg:  "listen tcp 127.0.0.1:8451: bind: address already in use",
			wantHit: true,
		},
		{
			name:    "macOS: address in use",
			errMsg:  "listen tcp6 [::1]:8451: bind: address in use",
			wantHit: true,
		},
		{
			name:    "Windows: socket address error",
			errMsg:  "listen tcp 127.0.0.1:8451: bind: Only one usage of each socket address (protocol/network address/port) is typically permitted.",
			wantHit: true,
		},
		{
			name:    "Permission denied (not port conflict)",
			errMsg:  "listen tcp 127.0.0.1:8451: bind: permission denied",
			wantHit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Contains(strings.ToLower(tt.errMsg), "in use") ||
				strings.Contains(tt.errMsg, "Only one usage of each socket address")
			if got != tt.wantHit {
				t.Errorf("端口冲突检测: err=%q, got=%v, want=%v", tt.errMsg, got, tt.wantHit)
			}
		})
	}
}
