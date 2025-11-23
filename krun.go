package main

import (
	"github.com/spf13/cobra"

	"github.com/aojea/krun/cmd/run"

	"k8s.io/klog/v2"
)

var rootCmd = &cobra.Command{
	Use:   "krun",
	Short: "krun is a tool to simplify AI/ML workflows on Kubernetes",
}

func main() {
	klog.InitFlags(nil)

	rootCmd.AddCommand(run.RunCmd)
	if err := rootCmd.Execute(); err != nil {
		panic(err)
	}

}
