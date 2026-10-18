// Command obfuscate-secret encodes the OAuth client secret for build-time
// injection. Its output is ciphertext:key in hex; both are embedded in the
// binary, so this only removes the plaintext string and is not encryption.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

func main() {
	secret := []byte(os.Getenv("WEATHERNEXT_OAUTH_CLIENT_SECRET"))
	if len(secret) == 0 {
		fmt.Fprintln(os.Stderr, "WEATHERNEXT_OAUTH_CLIENT_SECRET is required")
		os.Exit(1)
	}
	key := make([]byte, len(secret))
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		fmt.Fprintln(os.Stderr, "could not create build-time XOR key")
		os.Exit(1)
	}
	ciphertext := make([]byte, len(secret))
	for i := range secret {
		ciphertext[i] = secret[i] ^ key[i]
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s:%s\n", hex.EncodeToString(ciphertext), hex.EncodeToString(key))
}
