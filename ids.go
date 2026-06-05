package main

import (
	"crypto/rand"
	"strings"
)

// idAlphabet excludes easily-confused characters (0/O, 1/I/L) so IDs are easy
// to read aloud and type into a browser.
const idAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// genID returns a random stream ID of n chars. Short ids (local use) are easy
// to read aloud; long ids (public/LAN) act as unguessable bearer tokens, since
// the stream URL is the only access control.
//
// Bytes are drawn with rejection sampling (discarding v >= 248, the largest
// multiple of len(idAlphabet)=31 that fits in a byte) so every character is
// uniform — a plain v%31 would bias toward the first few letters.
func genID(n int) string {
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand should never fail; fall back to a fixed-but-valid id.
			return strings.Repeat("PIPER0", (n+5)/6)[:n]
		}
		for _, v := range buf {
			if v >= 248 { // reject the biased tail (248 = 8*31)
				continue
			}
			out = append(out, idAlphabet[int(v)%len(idAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

// validID reports whether s looks like one of our IDs (defensive routing).
func validID(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		ok := (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
