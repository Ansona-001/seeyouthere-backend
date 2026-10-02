package media

import (
	"bytes"
	"context"
	"testing"
)

// BenchmarkProcess_24MP measures the full decode+scale+encode pipeline for
// a 24 MP JPEG (6000x4000), the plan's target of well under 2.5s on 1 vCPU.
// Run with: go test ./internal/media/... -bench Process_24MP -benchtime=3x
func BenchmarkProcess_24MP(b *testing.B) {
	data, err := encodeJPEGFixture(6000, 4000)
	if err != nil {
		b.Fatalf("encode fixture: %v", err)
	}
	s, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	p := NewProcessor(1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srcPath, sniffed, err := s.Stage(bytes.NewReader(data))
		if err != nil {
			b.Fatalf("Stage: %v", err)
		}
		if _, err := p.Process(context.Background(), srcPath, sniffed, b.TempDir()); err != nil {
			b.Fatalf("Process: %v", err)
		}
	}
}
