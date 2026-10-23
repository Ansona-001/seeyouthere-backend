package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// Header byte 1 for MPEG-1/2/2.5 Layer III (no CRC) and Layer II.
const (
	hdrV1L3  = 0xFB
	hdrV2L3  = 0xF3
	hdrV25L3 = 0xE3
	hdrV1L2  = 0xFD
)

// mkFrame builds one frame: header b1 (version/layer), bitrate index, sample
// rate index, padding, zero payload of the given total length.
func mkFrame(b1 byte, brIdx, srIdx int, pad bool, length int) []byte {
	f := make([]byte, length)
	f[0], f[1] = 0xFF, b1
	f[2] = byte(brIdx<<4 | srIdx<<2)
	if pad {
		f[2] |= 2
	}
	f[3] = 0x00
	return f
}

// cbr128 is an MPEG-1 128 kbps 44.1 kHz frame: 144*128000/44100 = 417 bytes
// (418 padded), 1152 samples.
func cbr128(pad bool) []byte {
	n := 417
	if pad {
		n++
	}
	return mkFrame(hdrV1L3, 9, 0, pad, n)
}

// repeat concatenates n copies of f.
func repeat(f []byte, n int) []byte { return bytes.Repeat(f, n) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// id3v2 builds a tag with the given body size (syncsafe) and zero body.
func id3v2(size int) []byte {
	h := []byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7F), byte(size >> 14 & 0x7F), byte(size >> 7 & 0x7F), byte(size & 0x7F)}
	return cat(h, make([]byte, size))
}

func id3v1() []byte {
	t := make([]byte, 128)
	copy(t, "TAG")
	return t
}

// oneSecond is 39 CBR frames = 1018 ms.
var oneSecond = repeat(cbr128(false), 39)

func sanitize(t *testing.T, in []byte) ([]byte, AudioResult, error) {
	t.Helper()
	var out bytes.Buffer
	res, err := SanitizeMP3(bytes.NewReader(in), &out)
	return out.Bytes(), res, err
}

func TestSanitizeMP3_Accepted(t *testing.T) {
	vbr := func() []byte {
		var b []byte
		for i := 0; i < 60; i++ {
			br := 5 + i%8 // MPEG-1 bitrate indexes 5..12
			n := 144 * mpeg1Bitrates[br] * 1000 / 44100
			b = append(b, mkFrame(hdrV1L3, br, 0, false, n)...)
		}
		return b
	}()
	// 44 MPEG-2 frames at 22.05 kHz, 64 kbps: 72*64000/22050 = 208 bytes, 576 samples.
	mpeg2 := repeat(mkFrame(hdrV2L3, 8, 0, false, 208), 44)
	mixedPad := cat(repeat(cat(cbr128(false), cbr128(true)), 20))

	tests := []struct {
		name    string
		in      []byte
		want    []byte // expected output; nil = same as in
		wantMS  int32
		wantLen int64
	}{
		{"cbr 128k 44.1k", oneSecond, nil, 39 * 1152 * 1000 / 44100, int64(len(oneSecond))},
		{"mpeg-2 22.05k", mpeg2, nil, 44 * 576 * 1000 / 22050, int64(len(mpeg2))},
		{"mixed bitrates", vbr, nil, 60 * 1152 * 1000 / 44100, int64(len(vbr))},
		{"padding", mixedPad, nil, 40 * 1152 * 1000 / 44100, int64(len(mixedPad))},
		{"id3v2 prefix stripped", cat(id3v2(500), oneSecond), oneSecond, 39 * 1152 * 1000 / 44100, int64(len(oneSecond))},
		{"id3v2 empty body", cat(id3v2(0), oneSecond), oneSecond, 39 * 1152 * 1000 / 44100, int64(len(oneSecond))},
		{"id3v1 suffix stripped", cat(oneSecond, id3v1()), oneSecond, 39 * 1152 * 1000 / 44100, int64(len(oneSecond))},
		{"both tags", cat(id3v2(10), oneSecond, id3v1()), oneSecond, 39 * 1152 * 1000 / 44100, int64(len(oneSecond))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, res, err := sanitize(t, tt.in)
			if err != nil {
				t.Fatalf("SanitizeMP3: %v", err)
			}
			want := tt.want
			if want == nil {
				want = tt.in
			}
			if !bytes.Equal(out, want) {
				t.Errorf("output differs: got %d bytes, want %d", len(out), len(want))
			}
			if res.Bytes != tt.wantLen || res.Bytes != int64(len(out)) {
				t.Errorf("Bytes = %d, want %d (output %d)", res.Bytes, tt.wantLen, len(out))
			}
			if res.DurationMS != tt.wantMS {
				t.Errorf("DurationMS = %d, want %d", res.DurationMS, tt.wantMS)
			}
			// Idempotent: sanitising the output yields identical bytes.
			out2, res2, err := sanitize(t, out)
			if err != nil || !bytes.Equal(out2, out) || res2 != res {
				t.Errorf("second pass not idempotent: err=%v res=%+v", err, res2)
			}
		})
	}
}

func TestSanitizeMP3_RealLAMEFile(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "music-box.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	out, res, err := sanitize(t, in)
	if err != nil {
		t.Fatalf("SanitizeMP3: %v", err)
	}
	if len(out) == 0 || len(out) > len(in) || res.Bytes != int64(len(out)) {
		t.Errorf("bad result: in=%d out=%d res=%+v", len(in), len(out), res)
	}
	if res.DurationMS < 1000 || res.DurationMS > 600000 {
		t.Errorf("DurationMS = %d out of range", res.DurationMS)
	}
	out2, _, err := sanitize(t, out)
	if err != nil || !bytes.Equal(out, out2) {
		t.Errorf("not idempotent: %v", err)
	}
}

func TestSanitizeMP3_Rejected(t *testing.T) {
	good := cbr128(false)
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrInvalidAudio},
		{"one byte", []byte{0xFF}, ErrInvalidAudio},
		{"random text", []byte("this is definitely not an mp3 file at all"), ErrInvalidAudio},
		{"jpeg", mustJPEG(t, 8, 8), ErrInvalidAudio},
		{"png", mustPNG(t, 8, 8, false), ErrInvalidAudio},
		{"mixed sample rates", cat(oneSecond, repeat(mkFrame(hdrV1L3, 9, 1, false, 144*128000/48000), 5)), ErrInvalidAudio},
		{"mixed mpeg versions", cat(oneSecond, repeat(mkFrame(hdrV2L3, 8, 0, false, 208), 5)), ErrInvalidAudio},
		{"layer II", repeat(mkFrame(hdrV1L2, 9, 0, false, 417), 40), ErrInvalidAudio},
		{"reserved version", repeat(mkFrame(0xEB, 9, 0, false, 417), 40), ErrInvalidAudio},
		{"bitrate index 15", cat(oneSecond, mkFrame(hdrV1L3, 15, 0, false, 417)), ErrInvalidAudio},
		{"bitrate index 0 (free)", cat(oneSecond, mkFrame(hdrV1L3, 0, 0, false, 417)), ErrInvalidAudio},
		{"sample rate index 3", cat(oneSecond, mkFrame(hdrV1L3, 9, 3, false, 417)), ErrInvalidAudio},
		{"appended html", cat(oneSecond, []byte("<html><script>alert(1)</script></html>")), ErrInvalidAudio},
		{"appended zip", cat(oneSecond, []byte("PK\x03\x04"), make([]byte, 100)), ErrInvalidAudio},
		{"garbage between frames", cat(oneSecond, []byte{0x00}, oneSecond), ErrInvalidAudio},
		{"second id3v2 mid-stream", cat(oneSecond, id3v2(20), oneSecond), ErrInvalidAudio},
		{"id3v1 mid-stream", cat(oneSecond, id3v1(), oneSecond), ErrInvalidAudio},
		{"id3v1 followed by data", cat(oneSecond, id3v1(), []byte{0}), ErrInvalidAudio},
		{"bad syncsafe size", cat([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0x80, 0}, oneSecond), ErrInvalidAudio},
		{"id3v2 oversized", cat(id3v2(maxID3v2Bytes + 1)[:10], oneSecond), ErrInvalidAudio},
		{"id3v2 truncated body", id3v2(100)[:50], ErrInvalidAudio},
		{"id3v2 truncated header", []byte("ID3\x03\x00"), ErrInvalidAudio},
		{"id3v2 only", id3v2(10), ErrInvalidAudio},
		{"truncated final frame", oneSecond[:len(oneSecond)-1], ErrInvalidAudio},
		{"truncated header", cat(oneSecond, good[:3]), ErrInvalidAudio},
		{"too short", repeat(good, 10), ErrInvalidAudio},
		{"just under one second", repeat(good, 38), ErrInvalidAudio},
		// MPEG-2.5 8 kbps 8 kHz: 72 bytes, 72 ms per frame; 8334 frames = 600.048 s.
		{"over 10 minutes", repeat(mkFrame(hdrV25L3, 1, 2, false, 72), 8334), ErrAudioTooLong},
		// 13000 * 417 B = 5.4 MB but only about 5.2 minutes.
		{"over 5 MiB", repeat(good, 13000), ErrAudioTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := sanitize(t, tt.in)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}

	t.Run("exactly 10 minutes accepted", func(t *testing.T) {
		in := repeat(mkFrame(hdrV25L3, 1, 2, false, 72), 8333) // 599.976 s
		if _, res, err := sanitize(t, in); err != nil || res.DurationMS != 599976 {
			t.Errorf("res=%+v err=%v", res, err)
		}
	})
}

func TestSanitizeMP3_ReadErrorsPassThrough(t *testing.T) {
	t.Run("max bytes error", func(t *testing.T) {
		r := http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(oneSecond)), 1000)
		_, err := SanitizeMP3(r, io.Discard)
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) {
			t.Errorf("err = %v, want *http.MaxBytesError", err)
		}
	})
	t.Run("max bytes error inside id3v2 tag", func(t *testing.T) {
		r := http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(cat(id3v2(5000), oneSecond))), 100)
		_, err := SanitizeMP3(r, io.Discard)
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) {
			t.Errorf("err = %v, want *http.MaxBytesError", err)
		}
	})
	t.Run("arbitrary reader error", func(t *testing.T) {
		boom := errors.New("boom")
		r := io.MultiReader(bytes.NewReader(oneSecond[:100]), errReader{boom})
		if _, err := SanitizeMP3(r, io.Discard); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
	t.Run("writer error", func(t *testing.T) {
		boom := errors.New("disk full")
		if _, err := SanitizeMP3(bytes.NewReader(repeat(cbr128(false), 200)), errWriter{boom}); !errors.Is(err, boom) {
			t.Errorf("err = %v, want disk full", err)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

func FuzzSanitizeMP3(f *testing.F) {
	for _, s := range [][]byte{
		oneSecond,
		cat(id3v2(20), oneSecond, id3v1()),
		repeat(mkFrame(hdrV2L3, 8, 0, false, 208), 44),
		id3v2(10),
		[]byte("ID3"),
		{0xFF, 0xFB, 0x90, 0x00},
		nil,
	} {
		f.Add(s)
	}
	if b, err := os.ReadFile(filepath.Join("testdata", "music-box.mp3")); err == nil {
		f.Add(b[:min(len(b), 64<<10)])
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var out bytes.Buffer
		res, err := SanitizeMP3(bytes.NewReader(in), &out)
		if err != nil {
			return
		}
		if out.Len() > len(in) || res.Bytes != int64(out.Len()) {
			t.Fatalf("output %d bytes (res %d) from %d input bytes", out.Len(), res.Bytes, len(in))
		}
		var out2 bytes.Buffer
		if _, err := SanitizeMP3(bytes.NewReader(out.Bytes()), &out2); err != nil || !bytes.Equal(out.Bytes(), out2.Bytes()) {
			t.Fatalf("second pass differs: err=%v", err)
		}
	})
}

func TestAudioKey(t *testing.T) {
	ev, au := newIDs()
	want := "e/" + ev.String() + "/" + au.String() + "/audio.mp3"
	if got := audioKey(ev, au); got != want {
		t.Errorf("audioKey = %q, want %q", got, want)
	}
	if !ValidKey(want) {
		t.Error("audio key must satisfy ValidKey")
	}
}

func TestStore_AudioRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, backend := range []string{"mem", "disk"} {
		t.Run(backend, func(t *testing.T) {
			var blobs Blobs = newMemBlobs()
			if backend == "disk" {
				d, err := NewDiskBlobs(filepath.Join(t.TempDir(), "objects"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { d.Close() })
				blobs = d
			}
			s, err := NewStore(t.TempDir(), blobs)
			if err != nil {
				t.Fatal(err)
			}
			ev, au := newIDs()

			if _, err := s.OpenAudio(ctx, ev, au, ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("open before commit: %v, want ErrNotFound", err)
			}

			f, err := s.NewTempFile()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SanitizeMP3(bytes.NewReader(oneSecond), f); err != nil {
				t.Fatal(err)
			}
			path := f.Name()
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(path) != s.tmpDir() {
				t.Errorf("temp file %s not under tmp dir", path)
			}
			if err := s.CommitAudio(ctx, path, ev, au); err != nil {
				t.Fatalf("CommitAudio: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("CommitAudio should remove the staged file")
			}

			o, err := s.OpenAudio(ctx, ev, au, "")
			if err != nil {
				t.Fatalf("OpenAudio: %v", err)
			}
			got, _ := io.ReadAll(o.Body)
			_ = o.Body.Close()
			if !bytes.Equal(got, oneSecond) || o.Size != int64(len(oneSecond)) {
				t.Errorf("stored %d bytes (size %d), want %d", len(got), o.Size, len(oneSecond))
			}
			if o.ETag != "" {
				if _, err := s.OpenAudio(ctx, ev, au, o.ETag); !errors.Is(err, ErrNotModified) {
					t.Errorf("open with matching etag: %v, want ErrNotModified", err)
				}
			}

			if err := s.DeleteAudio(ctx, ev, au); err != nil {
				t.Fatalf("DeleteAudio: %v", err)
			}
			if _, err := s.OpenAudio(ctx, ev, au, ""); !errors.Is(err, ErrNotFound) {
				t.Errorf("open after delete: %v, want ErrNotFound", err)
			}
			if err := s.DeleteAudio(ctx, ev, au); err != nil {
				t.Errorf("second DeleteAudio: %v", err)
			}
		})
	}
}

func TestStore_CommitAudio_Failure(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	boom := errors.New("put failed")
	b.putErr = func(string) error { return boom }
	ev, au := newIDs()
	path := filepath.Join(s.tmpDir(), "staged.mp3")
	if err := os.WriteFile(path, oneSecond, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitAudio(context.Background(), path, ev, au); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want put failure", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("staged file must be removed on failure")
	}
	if b.count() != 0 {
		t.Error("no object should remain")
	}

	t.Run("missing staged file", func(t *testing.T) {
		if err := s.CommitAudio(context.Background(), filepath.Join(s.tmpDir(), "nope.mp3"), ev, au); err == nil {
			t.Error("expected error")
		}
	})
}

func TestStore_DeleteEventFiles_CoversAudio(t *testing.T) {
	ctx := context.Background()
	s, b := newTestStoreBlobs(t)
	evA, evB := newIDs()
	audioA, audioB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	b.seed(audioKey(evA, audioA), 10, 0)
	b.seed(audioKey(evB, audioB), 10, 0)

	if err := s.DeleteEventFiles(ctx, evA); err != nil {
		t.Fatal(err)
	}
	if b.has(audioKey(evA, audioA)) {
		t.Error("audio of deleted event remains")
	}
	if !b.has(audioKey(evB, audioB)) {
		t.Error("audio of another event was deleted")
	}
}

func TestReconcile_SweepsOrphanAudio(t *testing.T) {
	s, b := newTestStoreBlobs(t)
	ev, liveID := newIDs()
	orphanID := uuid.Must(uuid.NewV7())
	b.seed(audioKey(ev, liveID), 10, 2*orphanAge)
	b.seed(audioKey(ev, orphanID), 10, 2*orphanAge)

	st, err := s.Reconcile(context.Background(), func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
		m := map[uuid.UUID]bool{}
		for _, id := range ids {
			m[id] = id == liveID
		}
		return m, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.OrphansDeleted != 1 || !b.has(audioKey(ev, liveID)) || b.has(audioKey(ev, orphanID)) {
		t.Errorf("stats=%+v live=%v orphan=%v", st, b.has(audioKey(ev, liveID)), b.has(audioKey(ev, orphanID)))
	}
}
