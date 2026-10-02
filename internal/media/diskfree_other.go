//go:build !linux

package media

// freeBytes is a no-op outside Linux: production always runs on Linux
// (see the Dockerfile/compose), and dev/test machines on other platforms
// shouldn't fail CheckFree just because Statfs isn't wired up for them.
func freeBytes(string) (uint64, error) {
	const plentyFree = 1 << 40 // 1 TiB
	return plentyFree, nil
}
