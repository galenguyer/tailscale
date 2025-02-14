// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package taildrop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Tests "foo.jpg.deleted" marks (for Windows).
func TestDeletedMarkers(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{Dir: dir}

	nothingWaiting := func() {
		t.Helper()
		h.knownEmpty.Store(false)
		if h.HasFilesWaiting() {
			t.Fatal("unexpected files waiting")
		}
	}
	touch := func(base string) {
		t.Helper()
		if err := touchFile(filepath.Join(dir, base)); err != nil {
			t.Fatal(err)
		}
	}
	wantEmptyTempDir := func() {
		t.Helper()
		if fis, err := os.ReadDir(dir); err != nil {
			t.Fatal(err)
		} else if len(fis) > 0 && runtime.GOOS != "windows" {
			for _, fi := range fis {
				t.Errorf("unexpected file in tempdir: %q", fi.Name())
			}
		}
	}

	nothingWaiting()
	wantEmptyTempDir()

	touch("foo.jpg.deleted")
	nothingWaiting()
	wantEmptyTempDir()

	touch("foo.jpg.deleted")
	touch("foo.jpg")
	nothingWaiting()
	wantEmptyTempDir()

	touch("foo.jpg.deleted")
	touch("foo.jpg")
	wf, err := h.WaitingFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(wf) != 0 {
		t.Fatalf("WaitingFiles = %d; want 0", len(wf))
	}
	wantEmptyTempDir()

	touch("foo.jpg.deleted")
	touch("foo.jpg")
	if rc, _, err := h.OpenFile("foo.jpg"); err == nil {
		rc.Close()
		t.Fatal("unexpected foo.jpg open")
	}
	wantEmptyTempDir()

	// And verify basics still work in non-deleted cases.
	touch("foo.jpg")
	touch("bar.jpg.deleted")
	if wf, err := h.WaitingFiles(); err != nil {
		t.Error(err)
	} else if len(wf) != 1 {
		t.Errorf("WaitingFiles = %d; want 1", len(wf))
	} else if wf[0].Name != "foo.jpg" {
		t.Errorf("unexpected waiting file %+v", wf[0])
	}
	if rc, _, err := h.OpenFile("foo.jpg"); err != nil {
		t.Fatal(err)
	} else {
		rc.Close()
	}
}

func TestRedactErr(t *testing.T) {
	testCases := []struct {
		name string
		err  func() error
		want string
	}{
		{
			name: "PathError",
			err: func() error {
				return &os.PathError{
					Op:   "open",
					Path: "/tmp/sensitive.txt",
					Err:  fs.ErrNotExist,
				}
			},
			want: `open redacted.41360718: file does not exist`,
		},
		{
			name: "LinkError",
			err: func() error {
				return &os.LinkError{
					Op:  "symlink",
					Old: "/tmp/sensitive.txt",
					New: "/tmp/othersensitive.txt",
					Err: fs.ErrNotExist,
				}
			},
			want: `symlink redacted.41360718 redacted.6bcf093a: file does not exist`,
		},
		{
			name: "something else",
			err:  func() error { return errors.New("i am another error type") },
			want: `i am another error type`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// For debugging
			var i int
			for err := tc.err(); err != nil; err = errors.Unwrap(err) {
				t.Logf("%d: %T @ %p", i, err, err)
				i++
			}

			t.Run("Root", func(t *testing.T) {
				got := redactErr(tc.err()).Error()
				if got != tc.want {
					t.Errorf("err = %q; want %q", got, tc.want)
				}
			})
			t.Run("Wrapped", func(t *testing.T) {
				wrapped := fmt.Errorf("wrapped error: %w", tc.err())
				want := "wrapped error: " + tc.want

				got := redactErr(wrapped).Error()
				if got != want {
					t.Errorf("err = %q; want %q", got, want)
				}
			})
		})
	}
}
