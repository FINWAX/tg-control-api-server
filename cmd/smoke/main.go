// Command smoke verifies that libtdjson links and the TDLib native JSON
// interface round-trips end to end, using only synchronous methods
// (td_execute) so it needs no network, api_id, or authorization.
//
// It is the first build-prototype check for the TDLib + go-tdlib toolchain.
package main

import (
	"fmt"
	"os"

	"github.com/zelenin/go-tdlib/client"
)

func main() {
	// SetLogVerbosityLevel is synchronous; a clean return proves the cgo
	// bridge to libtdjson is wired up.
	if _, err := client.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{
		NewVerbosityLevel: 1,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "SetLogVerbosityLevel: %v\n", err)
		os.Exit(1)
	}

	// GetTextEntities exercises the full JSON round-trip: a td_api function
	// resolved by name, serialized, executed, and decoded back — the same
	// dynamic-dispatch path the gateway will use for /call.
	const sample = "Hello @durov, docs at https://core.telegram.org/tdlib #tdlib"
	entities, err := client.GetTextEntities(&client.GetTextEntitiesRequest{Text: sample})
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetTextEntities: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("libtdjson OK. binding targets TDLib %s\n", client.TDLIB_VERSION)
	fmt.Printf("parsed %d entities from sample text:\n", len(entities.Entities))
	for _, e := range entities.Entities {
		fmt.Printf("  offset=%2d len=%2d type=%T\n", e.Offset, e.Length, e.Type)
	}
}
