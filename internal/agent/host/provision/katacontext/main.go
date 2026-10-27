// Command katacontext writes the kata image's build context (provision.WriteKataContext) to a
// directory and prints its contextSHA256, so a person (make kata-context) and CI build the same
// context factory-agent does: docker build <dir>.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ContainerSolutions/factory/internal/agent/host/provision"
)

func main() {
	out := flag.String("out", "", "the directory to write the build context to (made when missing)")
	flag.Parse()
	if *out == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: katacontext -out <dir>")
		os.Exit(2)
	}
	sum, err := provision.WriteKataContext(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "katacontext:", err)
		os.Exit(1)
	}
	fmt.Printf("%s  %s\n", sum, *out)
}
