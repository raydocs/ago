package main

import (
	"fmt"
	"os"

	"claudexflow/internal/agolocalexec"
)

func main() {
	// ExtraFiles[0] is descriptor 3 by Go's exec contract.
	liveness := os.NewFile(3, "broker-liveness")
	if liveness == nil {
		fmt.Fprintln(os.Stderr, "ago-supervisor: missing broker liveness descriptor")
		os.Exit(125)
	}
	defer liveness.Close()
	controls := os.NewFile(4, "broker-controls")
	events := os.NewFile(5, "broker-events")
	providerRequests := os.NewFile(6, "provider-requests")
	providerResponses := os.NewFile(7, "provider-responses")
	if controls != nil {
		defer controls.Close()
	}
	if events != nil {
		defer events.Close()
	}
	if err := agolocalexec.RunSupervisorProviderSession(os.Stdin, os.Stdout, liveness, controls, events, providerRequests, providerResponses); err != nil {
		fmt.Fprintln(os.Stderr, "ago-supervisor:", err)
		os.Exit(125)
	}
}
