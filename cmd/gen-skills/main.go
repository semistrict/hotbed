// Command gen-skills writes generated skill reference docs from hotbed.proto descriptors.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/semistrict/hotbed/internal/apicmd"
	"github.com/semistrict/hotbed/internal/apicmd/spec"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1cli"
)

func main() {
	s, err := spec.Load(hotbedv1cli.Descriptors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load descriptors: %v\n", err)
		os.Exit(1)
	}
	out := filepath.Join("hotbed", "references", "commands.md")
	if err := os.WriteFile(out, []byte(apicmd.RenderCommandReference(s)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", out, err)
		os.Exit(1)
	}
}
