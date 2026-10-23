package media

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

const (
	// MaxAudioBytes is the largest sanitised MP3 we store (matches the
	// size_bytes CHECK on the audio table).
	MaxAudioBytes = 5 << 20
	// MinAudioDuration and MaxAudioDuration bound the accepted playing time.
	MinAudioDuration = time.Second
	MaxAudioDuration = 10 * time.Minute

	// maxID3v2Bytes caps a leading ID3v2 tag body we are willing to skip.
	maxID3v2Bytes = 1 << 20
	audioFile     = "audio.mp3"

	// maxFrameBytes is the longest legal Layer III frame (MPEG-1, 320 kbps,
	// 32 kHz, padded = 1441 bytes), rounded up.
	maxFrameBytes = 1536
)

var (
	// ErrInvalidAudio is returned when the input is not a clean MP3: any byte
	// that is not part of a valid MPEG Layer III frame (other than a leading
	// ID3v2 or trailing ID3v1 tag), mixed stream parameters, truncation, a
	// duration under MinAudioDuration.
	ErrInvalidAudio = errors.New("media: invalid audio")
	// ErrAudioTooLong is returned when the audio plays longer than MaxAudioDuration.
	ErrAudioTooLong = errors.New("media: audio too long")
	// ErrAudioTooLarge is returned when the sanitised output would exceed MaxAudioBytes.
	ErrAudioTooLarge = errors.New("media: audio too large")
)

// AudioResult describes the sanitised output of SanitizeMP3.
type AudioResult struct {
	Bytes      int64
	DurationMS int32
}

// audioKey is the object key of an audio item's MP3. Built only from parsed
// UUIDs, never from request text.
func audioKey(eventID, audioID uuid.UUID) string {
	return fmt.Sprintf("e/%s/%s/%s", eventID, audioID, audioFile)
}

// Layer III bitrates in kbps, indexed by the header's bitrate index (1-14),
// and sample rates in Hz by version bits (0 = MPEG 2.5, 2 = MPEG 2,
// 3 = MPEG 1) and sample-rate index (0-2).
var (
	mpeg1Bitrates = [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mpeg2Bitrates = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	sampleRates   = [4][3]int{{11025, 12000, 8000}, {}, {22050, 24000, 16000}, {44100, 48000, 32000}}
)

// frameHeader is a validated MPEG Layer III frame header.
type frameHeader struct {
	version    int
	sampleRate int
	samples    int // samples per channel in the frame
	length     int // total frame bytes, header included
}

// parseFrameHeader validates the 4 header bytes; ok is false for anything
// that is not a legal Layer III frame.
func parseFrameHeader(h []byte) (frameHeader, bool) {
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return frameHeader{}, false
	}
	version := int(h[1]>>3) & 3
	if version == 1 || (h[1]>>1)&3 != 1 { // reserved version, or not Layer III
		return frameHeader{}, false
	}
	brIdx, srIdx := int(h[2]>>4), int(h[2]>>2)&3
	if brIdx == 0 || brIdx == 15 || srIdx == 3 {
		return frameHeader{}, false
	}
	fh := frameHeader{version: version, sampleRate: sampleRates[version][srIdx]}
	pad := int(h[2]>>1) & 1
	if version == 3 {
		fh.samples = 1152
		fh.length = 144*mpeg1Bitrates[brIdx]*1000/fh.sampleRate + pad
	} else {
		fh.samples = 576
		fh.length = 72*mpeg2Bitrates[brIdx]*1000/fh.sampleRate + pad
	}
	return fh, true
}

// audioReadErr maps a premature end of input to ErrInvalidAudio and passes
// every other read error (notably *http.MaxBytesError) through unchanged.
func audioReadErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrInvalidAudio
	}
	return err
}

// SanitizeMP3 streams an MP3 from r and writes only its validated MPEG
// Layer III frames to w, dropping a leading ID3v2 tag and a trailing ID3v1
// tag. It is deliberately strict: every other byte (garbage between frames,
// appended data, a truncated last frame, mixed MPEG version or sample rate,
// other layers) yields ErrInvalidAudio, so no foreign payload is ever stored.
// Duration is the exact sum of frame durations and must be within
// [MinAudioDuration, MaxAudioDuration] (ErrAudioTooLong above). The caller
// bounds the input size (http.MaxBytesReader); read errors from r are
// returned unchanged. Memory is one bufio buffer plus one frame. On error w
// may hold partial output and must be discarded.
func SanitizeMP3(r io.Reader, w io.Writer) (AudioResult, error) {
	br := bufio.NewReaderSize(r, 4096)
	bw := bufio.NewWriterSize(w, 32<<10)

	if err := skipID3v2(br); err != nil {
		return AudioResult{}, err
	}

	var (
		buf        [maxFrameBytes]byte
		first      frameHeader
		frames     int64
		samples    int64
		maxSamples int64 // set once the sample rate is known
		out        int64
	)
	for {
		head, err := br.Peek(4)
		if len(head) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if len(head) == 4 && head[0] == 'T' {
			// A trailing ID3v1 tag is exactly 128 bytes and the end of input.
			tail, perr := br.Peek(129)
			if len(tail) == 128 && errors.Is(perr, io.EOF) && string(tail[:3]) == "TAG" {
				break
			}
			if perr != nil && !errors.Is(perr, io.EOF) {
				return AudioResult{}, perr
			}
		}
		if len(head) < 4 {
			if err != nil && !errors.Is(err, io.EOF) {
				return AudioResult{}, err
			}
			return AudioResult{}, ErrInvalidAudio
		}
		fh, ok := parseFrameHeader(head)
		if !ok {
			return AudioResult{}, ErrInvalidAudio
		}
		if frames == 0 {
			first = fh
			maxSamples = int64(MaxAudioDuration/time.Second) * int64(fh.sampleRate)
		} else if fh.version != first.version || fh.sampleRate != first.sampleRate {
			return AudioResult{}, ErrInvalidAudio
		}
		samples += int64(fh.samples)
		if samples > maxSamples {
			return AudioResult{}, ErrAudioTooLong
		}
		out += int64(fh.length)
		if out > MaxAudioBytes {
			return AudioResult{}, ErrAudioTooLarge
		}
		if _, err := io.ReadFull(br, buf[:fh.length]); err != nil {
			return AudioResult{}, audioReadErr(err)
		}
		if _, err := bw.Write(buf[:fh.length]); err != nil {
			return AudioResult{}, fmt.Errorf("media: write audio: %w", err)
		}
		frames++
	}

	if frames == 0 {
		return AudioResult{}, ErrInvalidAudio
	}
	ms := samples * 1000 / int64(first.sampleRate)
	if ms < MinAudioDuration.Milliseconds() {
		return AudioResult{}, fmt.Errorf("%w: shorter than %s", ErrInvalidAudio, MinAudioDuration)
	}
	if err := bw.Flush(); err != nil {
		return AudioResult{}, fmt.Errorf("media: write audio: %w", err)
	}
	return AudioResult{Bytes: out, DurationMS: int32(ms)}, nil
}

// skipID3v2 discards a leading ID3v2 tag, if any. The size is four syncsafe
// bytes and is capped; a malformed or truncated tag is ErrInvalidAudio.
func skipID3v2(br *bufio.Reader) error {
	head, err := br.Peek(3)
	if len(head) < 3 {
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return ErrInvalidAudio
	}
	if string(head) != "ID3" {
		return nil
	}
	var h [10]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return audioReadErr(err)
	}
	if h[3] == 0xFF || h[4] == 0xFF || h[6]|h[7]|h[8]|h[9] >= 0x80 {
		return ErrInvalidAudio
	}
	size := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
	if h[5]&0x10 != 0 { // footer present
		size += 10
	}
	if size > maxID3v2Bytes {
		return ErrInvalidAudio
	}
	if _, err := io.CopyN(io.Discard, br, size); err != nil {
		return audioReadErr(err)
	}
	return nil
}
