package audiocheck

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// pcmHasher is the stdout sink for the decode pass: it hashes the PCM as it streams past
// and counts its bytes, so a 10-minute hi-res track is never held in memory.
type pcmHasher struct {
	h hash.Hash
	n int64
}

func newPCMHasher() *pcmHasher { return &pcmHasher{h: sha256.New()} }

func (p *pcmHasher) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	return p.h.Write(b)
}

func (p *pcmHasher) sum() string { return hex.EncodeToString(p.h.Sum(nil)) }
