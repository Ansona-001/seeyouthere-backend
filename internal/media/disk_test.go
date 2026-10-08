package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDisk(t *testing.T) (*DiskBlobs, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "objects")
	d, err := NewDiskBlobs(dir)
	if err != nil {
		t.Fatalf("NewDiskBlobs: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dir
}

func diskPut(t *testing.T, d *DiskBlobs, key, content string) {
	t.Helper()
	if err := d.Put(context.Background(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
}

func readObject(t *testing.T, o *Object) string {
	t.Helper()
	defer o.Body.Close()
	b, err := io.ReadAll(o.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestValidKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"e/0191a2b3-c4d5-7e6f-8a9b-0c1d2e3f4a5b/0191a2b3-c4d5-7e6f-8a9b-0c1d2e3f4a5b/480.jpg", true},
		{"t/abc/3/1920.jpg", true},
		{"a", true},
		{"0", true},
		{"a_b-c.d/e", true},
		{"", false},
		{"/abs/key.jpg", false},
		{"../x.jpg", false},
		{"a/../b", false},
		{"a..b", false},
		{"a//b", false},
		{"A/b.jpg", false},
		{"e/Upper.jpg", false},
		{"-leading", false},
		{".hidden", false},
		{"a b", false},
		{"a\\b", false},
		{"a\x00b", false},
		{"é", false},
		{"a?b", false},
		{strings.Repeat("a", 201), true},
		{strings.Repeat("a", 202), false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.key), func(t *testing.T) {
			if got := ValidKey(tt.key); got != tt.want {
				t.Errorf("ValidKey = %v, want %v", got, tt.want)
			}
		})
	}
	if !ValidPrefix("") || !ValidPrefix("e/abc/") || ValidPrefix("../") || ValidPrefix("E/") {
		t.Error("ValidPrefix mismatch")
	}
}

func TestDiskBlobs_PutGetOverwrite(t *testing.T) {
	d, dir := newTestDisk(t)
	ctx := context.Background()
	diskPut(t, d, "e/aa/bb/480.jpg", "first")

	o, err := d.Get(ctx, "e/aa/bb/480.jpg", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if o.Size != 5 || !strings.HasPrefix(o.ETag, `"`) {
		t.Errorf("size %d etag %q", o.Size, o.ETag)
	}
	if got := readObject(t, o); got != "first" {
		t.Errorf("body = %q", got)
	}

	diskPut(t, d, "e/aa/bb/480.jpg", "second!")
	o, err = d.Get(ctx, "e/aa/bb/480.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readObject(t, o); got != "second!" {
		t.Errorf("overwrite body = %q", got)
	}

	entries, _ := os.ReadDir(filepath.Join(dir, "e", "aa", "bb"))
	if len(entries) != 1 {
		t.Errorf("expected only the object file, got %d entries (temp file left behind?)", len(entries))
	}
}

func TestDiskBlobs_Put_Errors(t *testing.T) {
	d, dir := newTestDisk(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		key  string
		body string
		size int64
	}{
		{"invalid key", context.Background(), "../escape.jpg", "x", 1},
		{"uppercase key", context.Background(), "E/x.jpg", "x", 1},
		{"short body", context.Background(), "e/a.jpg", "abc", 5},
		{"long body", context.Background(), "e/a.jpg", "abcdef", 5},
		{"cancelled", cancelled, "e/a.jpg", "abc", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := d.Put(tt.ctx, tt.key, strings.NewReader(tt.body), tt.size); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	// No partial object or temp file may remain from the failures above.
	var left []string
	_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, _ error) error {
		if e != nil && !e.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("files left after failed puts: %v", left)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.jpg")); err == nil {
		t.Error("traversal key wrote outside the root")
	}
}

func TestDiskBlobs_Get_Errors(t *testing.T) {
	d, _ := newTestDisk(t)
	ctx := context.Background()
	diskPut(t, d, "e/aa/bb/480.jpg", "data")
	o, err := d.Get(ctx, "e/aa/bb/480.jpg", "")
	if err != nil {
		t.Fatal(err)
	}
	etag := o.ETag
	o.Body.Close()

	tests := []struct {
		name    string
		key     string
		inm     string
		wantErr error
		invalid bool
	}{
		{"missing", "e/aa/bb/1080.jpg", "", ErrNotFound, false},
		{"directory", "e/aa", "", ErrNotFound, false},
		{"etag match", "e/aa/bb/480.jpg", etag, ErrNotModified, false},
		{"stale etag", "e/aa/bb/480.jpg", `"zz"`, nil, false},
		{"traversal", "../x", "", nil, true},
		{"absolute", "/etc/passwd", "", nil, true},
		{"uppercase", "E/aa", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := d.Get(ctx, tt.key, tt.inm)
			if o != nil {
				o.Body.Close()
			}
			switch {
			case tt.invalid:
				if err == nil || errors.Is(err, ErrNotFound) {
					t.Errorf("err = %v, want invalid-key error", err)
				}
			case !errors.Is(err, tt.wantErr):
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDiskBlobs_Delete(t *testing.T) {
	d, dir := newTestDisk(t)
	ctx := context.Background()
	diskPut(t, d, "e/aa/bb/480.jpg", "1")
	diskPut(t, d, "e/aa/bb/1080.jpg", "2")
	diskPut(t, d, "e/aa/cc/480.jpg", "3")

	if err := d.Delete(ctx, []string{"e/aa/bb/480.jpg", "e/aa/bb/1080.jpg", "e/aa/nope.jpg"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.Get(ctx, "e/aa/cc/480.jpg", ""); err != nil {
		t.Errorf("unrelated object removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "e", "aa", "bb")); !os.IsNotExist(err) {
		t.Error("empty directory should be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "e", "aa")); err != nil {
		t.Error("non-empty directory must stay")
	}

	t.Run("invalid key aborts before deleting anything", func(t *testing.T) {
		if err := d.Delete(ctx, []string{"e/aa/cc/480.jpg", "../x"}); err == nil {
			t.Fatal("expected error")
		}
		if _, err := d.Get(ctx, "e/aa/cc/480.jpg", ""); err != nil {
			t.Errorf("valid key deleted despite batch rejection: %v", err)
		}
	})

	t.Run("more than 1000 keys", func(t *testing.T) {
		keys := make([]string, 1500)
		for i := range keys {
			keys[i] = fmt.Sprintf("m/%d.jpg", i)
			diskPut(t, d, keys[i], "x")
		}
		if err := d.Delete(ctx, keys); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		var n int
		if err := d.List(ctx, "m/", func(p []ObjectInfo) error { n += len(p); return nil }); err != nil || n != 0 {
			t.Errorf("list after delete: n=%d err=%v", n, err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		if err := d.Delete(c, []string{"e/aa/cc/480.jpg"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestDiskBlobs_List(t *testing.T) {
	d, _ := newTestDisk(t)
	ctx := context.Background()
	for _, k := range []string{"e/aa/m1/480.jpg", "e/aa/m1/1080.jpg", "e/aa/m2/480.jpg", "e/ab/m3/480.jpg", "t/x/1/480.jpg"} {
		diskPut(t, d, k, "data")
	}

	collect := func(prefix string) []string {
		var keys []string
		if err := d.List(ctx, prefix, func(p []ObjectInfo) error {
			for _, o := range p {
				if o.Size != 4 || o.LastModified.IsZero() {
					t.Errorf("bad info %+v", o)
				}
				keys = append(keys, o.Key)
			}
			return nil
		}); err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		return keys
	}

	tests := []struct {
		prefix string
		want   string
	}{
		{"", "e/aa/m1/1080.jpg e/aa/m1/480.jpg e/aa/m2/480.jpg e/ab/m3/480.jpg t/x/1/480.jpg"},
		{"e/", "e/aa/m1/1080.jpg e/aa/m1/480.jpg e/aa/m2/480.jpg e/ab/m3/480.jpg"},
		{"e/aa/", "e/aa/m1/1080.jpg e/aa/m1/480.jpg e/aa/m2/480.jpg"},
		{"e/aa/m1/", "e/aa/m1/1080.jpg e/aa/m1/480.jpg"},
		{"e/aa/m1/4", "e/aa/m1/480.jpg"},
		{"e/a", "e/aa/m1/1080.jpg e/aa/m1/480.jpg e/aa/m2/480.jpg e/ab/m3/480.jpg"},
		{"e/ab", "e/ab/m3/480.jpg"},
		{"e/zz/", ""},
		{"nothing/here/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			if got := strings.Join(collect(tt.prefix), " "); got != tt.want {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}

	t.Run("invalid prefix", func(t *testing.T) {
		if err := d.List(ctx, "../", func([]ObjectInfo) error { return nil }); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("callback error stops listing", func(t *testing.T) {
		errStop := errors.New("stop")
		calls := 0
		err := d.List(ctx, "", func([]ObjectInfo) error { calls++; return errStop })
		if !errors.Is(err, errStop) || calls != 1 {
			t.Errorf("err = %v calls = %d", err, calls)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		if err := d.List(c, "", func([]ObjectInfo) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestDiskBlobs_List_PagesAndTempFiles(t *testing.T) {
	d, dir := newTestDisk(t)
	ctx := context.Background()
	const n = 2300
	for i := 0; i < n; i++ {
		diskPut(t, d, fmt.Sprintf("e/aa/%05d.jpg", i), "x")
	}
	// An in-progress Put's temp file must stay invisible.
	if err := os.WriteFile(filepath.Join(dir, "e", "aa", "zz.jpg.tmp-123"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sizes []int
	var last string
	total := 0
	err := d.List(ctx, "e/", func(p []ObjectInfo) error {
		sizes = append(sizes, len(p))
		for _, o := range p {
			if o.Key <= last {
				t.Fatalf("keys not ordered: %s after %s", o.Key, last)
			}
			last = o.Key
		}
		total += len(p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Errorf("total = %d, want %d", total, n)
	}
	if fmt.Sprint(sizes) != "[1000 1000 300]" {
		t.Errorf("page sizes = %v, want [1000 1000 300]", sizes)
	}
	if _, err := d.Get(ctx, "e/aa/zz.jpg.tmp-123", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("temp file readable: %v", err)
	}
}

func TestDiskBlobs_ListDuringDelete(t *testing.T) {
	d, _ := newTestDisk(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		diskPut(t, d, fmt.Sprintf("e/ev/m%02d/480.jpg", i), "x")
	}
	// Mirrors DeleteEventFiles: delete each page while the walk continues.
	err := d.List(ctx, "e/ev/", func(p []ObjectInfo) error {
		keys := make([]string, len(p))
		for i, o := range p {
			keys[i] = o.Key
		}
		return d.Delete(ctx, keys)
	})
	if err != nil {
		t.Fatalf("List with deletes: %v", err)
	}
	var left int
	_ = d.List(ctx, "", func(p []ObjectInfo) error { left += len(p); return nil })
	if left != 0 {
		t.Errorf("%d objects left", left)
	}
}

func TestDiskBlobs_SymlinkCannotEscapeRoot(t *testing.T) {
	d, dir := newTestDisk(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.jpg"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if o, err := d.Get(context.Background(), "link/secret.jpg", ""); err == nil {
		o.Body.Close()
		t.Error("Get followed a symlink out of the root")
	}
	if err := d.Put(context.Background(), "link/new.jpg", bytes.NewReader([]byte("x")), 1); err == nil {
		t.Error("Put followed a symlink out of the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.jpg")); err == nil {
		t.Error("file created outside the root")
	}
}

func TestDiskBlobs_ETagChangesOnOverwrite(t *testing.T) {
	d, _ := newTestDisk(t)
	ctx := context.Background()
	diskPut(t, d, "e/a.jpg", "one")
	o1, _ := d.Get(ctx, "e/a.jpg", "")
	e1 := o1.ETag
	o1.Body.Close()
	time.Sleep(20 * time.Millisecond)
	diskPut(t, d, "e/a.jpg", "two")
	o2, _ := d.Get(ctx, "e/a.jpg", "")
	e2 := o2.ETag
	o2.Body.Close()
	if e1 == e2 {
		t.Error("ETag unchanged after overwrite")
	}
}
