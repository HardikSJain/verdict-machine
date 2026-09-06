// Command algo is the Verdict Machine binary: ingest, backtest, evening, replay, verify.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "algo",
		Short:         "Verdict Machine: a daily-bar NSE strategy lab that earns autonomy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "algo "+version)
			return err
		},
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
