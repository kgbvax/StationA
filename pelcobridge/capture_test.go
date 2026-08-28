// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// captureStdout runs f with os.Stdout redirected into a buffer and returns
// what f printed (test helper shared by the main-package tests).
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	f()

	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}
