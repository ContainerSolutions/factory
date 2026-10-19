// Command credhelper is the run's git credential helper for the effector tests, as
// `factory-agent fx git-credential` runs it (gitcred.Helper), built without -race so that
// git's many helper calls stay cheap.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/compliance-framework/factory/internal/agent/gitcred"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "fx" || os.Args[2] != "git-credential" {
		fmt.Fprintln(os.Stderr, "usage: credhelper fx git-credential get|store|erase")
		os.Exit(2)
	}
	if err := gitcred.Helper(context.Background(), os.Args[3:], os.Stdin, os.Stdout, os.Getenv(gitcred.EnvSocket)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
}
