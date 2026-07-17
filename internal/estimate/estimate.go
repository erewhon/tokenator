// Package estimate holds the block-size token estimator.
//
// Block-level token counts are ESTIMATES: bytes/4 is a crude but serviceable
// proxy for Claude tokenizers on mixed code/prose. Attribution reports must
// therefore present block numbers as shares of metered totals (see
// store.Calibrate), never as authoritative counts. Swap the estimator here
// if per-block absolute numbers ever start to matter.
package estimate

import (
	"crypto/sha256"
	"encoding/hex"
)

// Tokens estimates the token count of a byte length.
func Tokens(byteLen int) int64 {
	return int64((byteLen + 3) / 4)
}

// Hash returns a truncated (64-bit) content hash — enough to detect
// repeated content, deliberately not enough to reconstruct anything.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
