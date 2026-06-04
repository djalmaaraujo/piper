package main

import "crypto/rand"

// idAlphabet excludes easily-confused characters (0/O, 1/I/L) so IDs are easy
// to read aloud and type into a browser.
const idAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// genID returns a short random stream ID like "USHS82".
func genID() string {
	const n = 6
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail; fall back to a fixed-but-valid id.
		return "PIPER0"
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = idAlphabet[int(v)%len(idAlphabet)]
	}
	return string(out)
}

// validID reports whether s looks like one of our IDs (defensive routing).
func validID(s string) bool {
	if len(s) == 0 || len(s) > 16 {
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
