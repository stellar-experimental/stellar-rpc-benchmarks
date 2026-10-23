package run

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// The packfile trailer, mirrored from stellar-rpc
// cmd/stellar-rpc/internal/rpcv2/packfile/packfile.go at 06927e1a (#1128):
// magic and version at :9 and :13, and in the last four trailer bytes the
// CRC32C (index.go:39) of the bytes before them (tOffCRC, :48). The trailer
// is 76 bytes before #1128 (06927e1a^:15) and 44 from it on (:14).
const (
	packMagic   = 0x48434C53
	packVersion = 1
)

// packGenerations are the pack generations the runner tells apart, by trailer
// size.
var packGenerations = []struct {
	gen         plan.PackGeneration
	trailerSize int
}{
	{plan.PackGenV4, 76},
	{plan.PackGenV5, 44},
}

var crc32c = crc32.MakeTable(crc32.Castagnoli)

// errPackGeneration marks a dataset whose packs the binary under test cannot
// read; Execute stops the walk on it.
var errPackGeneration = errors.New("pack generation mismatch")

// checksPackGeneration reports whether a dataset kind reads packs another
// binary wrote. A backfill or a fixture is written by the binary under test.
func checksPackGeneration(kind string) bool {
	switch kind {
	case config.KindPacksLocal, config.KindPacksGS, config.KindPacksS3:
		return true
	}
	return false
}

// CheckPackGeneration fails unless the first ledger pack under the dataset's
// root is in generation gen, which binary (stellar-rpc-<sha8>) reads.
func CheckPackGeneration(d *plan.DatasetSpec, gen plan.PackGeneration, binary string) error {
	path, err := firstPack(filepath.Join(d.Root, "ledgers"))
	if err != nil {
		return fmt.Errorf("dataset %s: %w", d.Name, err)
	}
	got, err := PackGenerationOf(path)
	if err != nil {
		return fmt.Errorf("dataset %s: %w", d.Name, err)
	}
	if got == gen {
		return nil
	}
	msg := fmt.Sprintf("dataset %s is packs-%s (%s); stellar-rpc %s reads packs-%s (stellar-rpc#1128)",
		d.Name, got, path, strings.TrimPrefix(filepath.Base(binary), "stellar-rpc-"), gen)
	if d.Kind != config.KindPacksLocal {
		msg += fmt.Sprintf(" — when %s holds an earlier fetch, rm -rf it to fetch %s", d.Root, d.Location)
	}
	return fmt.Errorf("%w: %s", errPackGeneration, msg)
}

// PackGenerationOf reports the pack generation of the pack file at path from
// its trailer, which must match exactly one known generation.
func PackGenerationOf(path string) (plan.PackGeneration, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	tail := make([]byte, min(fi.Size(), int64(packGenerations[0].trailerSize)))
	if _, err := f.ReadAt(tail, fi.Size()-int64(len(tail))); err != nil {
		return "", fmt.Errorf("read the trailer of %s: %w", path, err)
	}
	var matches []plan.PackGeneration
	for _, g := range packGenerations {
		if g.trailerSize <= len(tail) && validTrailer(tail[len(tail)-g.trailerSize:]) {
			matches = append(matches, g.gen)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%s has %d valid packs-v4/packs-v5 trailers, want exactly one", path, len(matches))
	}
	return matches[0], nil
}

func validTrailer(tb []byte) bool {
	n := len(tb)
	return binary.LittleEndian.Uint32(tb) == packMagic && tb[4] == packVersion &&
		crc32.Checksum(tb[:n-4], crc32c) == binary.LittleEndian.Uint32(tb[n-4:])
}

// firstPack is the lexically first .pack file under dir.
func firstPack(dir string) (string, error) {
	var first string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() && strings.HasSuffix(path, ".pack") {
			first = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", fmt.Errorf("no .pack file under %s", dir)
	}
	return first, nil
}
