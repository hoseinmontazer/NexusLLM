// genkey generates a NexusLLM API key and prints its SHA-256 hash.
// Usage: go run ./tools/genkey/main.go
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	raw := "nxs_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])

	fmt.Println("─────────────────────────────────────────────────────")
	fmt.Println("NexusLLM API Key")
	fmt.Println("─────────────────────────────────────────────────────")
	fmt.Printf("Key (save this — shown once):  %s\n", raw)
	fmt.Printf("Hash (store in DB key_hash):   %s\n", hash)
	fmt.Printf("Prefix (display):              %s\n", raw[:12])
	fmt.Println("─────────────────────────────────────────────────────")
	fmt.Println("INSERT INTO api_keys (team_id, name, key_hash, key_prefix)")
	fmt.Printf("VALUES ('<team-uuid>', 'my-key', '%s', '%s');\n", hash, raw[:12])
}
