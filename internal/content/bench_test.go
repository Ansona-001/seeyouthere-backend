package content

import (
	"encoding/json"
	"testing"
)

func BenchmarkValidateManifestV2(b *testing.B) {
	for name, fx := range v2Fixtures() {
		raw := advMarshal(b, fx)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				if _, err := ValidateManifest(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkValidateManifestV2Rejected measures the failure path (the issue
// list is capped, so a hostile manifest must not cost more than a valid one).
func BenchmarkValidateManifestV2Rejected(b *testing.B) {
	raw := advMutate(b, oliveGrove(), func(m obj) {
		m["layout"] = "nope"
		colorsAt(m, 0)["text"] = "red"
		layerAt(m, 2)["art"] = "nope"
		layerAt(m, 1)["opacity"] = 7
	})
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		if _, err := ValidateManifest(raw); err == nil {
			b.Fatal("accepted")
		}
	}
}

func BenchmarkResolveV2(b *testing.B) {
	for name, fx := range v2Fixtures() {
		m, err := ValidateManifest(advMarshal(b, fx))
		if err != nil {
			b.Fatal(err)
		}
		ov, _ := json.Marshal(Overrides{Palette: m.Palettes[len(m.Palettes)-1].ID})
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = ResolveTheme(m, ov, "/media/abc/background")
			}
		})
	}
}

// BenchmarkResolveV2JSON is resolve plus the response encoding, the work a
// public event request does per theme.
func BenchmarkResolveV2JSON(b *testing.B) {
	for name, fx := range v2Fixtures() {
		m, err := ValidateManifest(advMarshal(b, fx))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := json.Marshal(ResolveTheme(m, nil, "/media/abc/background")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
