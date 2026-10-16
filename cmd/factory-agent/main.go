// Command factory-agent is the CCF Factory agent. Subcommands land in their own files
// (one file per subcommand, registered from main) as the workstreams add them.
package main

import (
	"fmt"

	"github.com/compliance-framework/factory/internal/version"
)

func main() {
	fmt.Println(version.String("factory-agent"))
}
