package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/wanglongan587/cloud/internal/contract"
)

func main() {
	b, e := json.MarshalIndent(contract.Document(), "", "  ")
	if e == nil {
		e = os.MkdirAll("api", 0o755)
	}
	if e == nil {
		e = os.WriteFile("api/openapi.json", append(b, '\n'), 0o600)
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
