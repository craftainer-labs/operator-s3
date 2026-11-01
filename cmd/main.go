// Command manager runs the operator. All logic lives in internal/app; this
// file only wires it to the process (signals, arguments, exit code), which is
// why the coverage gate leaves cmd/ out.
package main

import (
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/craftainer/template-k8s-operator/internal/app"
)

func main() {
	if err := app.Run(ctrl.SetupSignalHandler(), os.Args[1:], ctrl.GetConfig, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
