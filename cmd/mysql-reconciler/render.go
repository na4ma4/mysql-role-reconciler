package main

import (
	"fmt"
	"os"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var renderCmd = &cobra.Command{
	Use:   "render -c config.yaml",
	Short: "Render resolved config as YAML",
	Long: `Read the configuration file, resolve all !include directives, and output
the fully-resolved YAML to stdout. Useful for debugging and verifying that
include tags are expanded correctly.`,
	Args:   cobra.NoArgs,
	RunE:   runRender,
	Hidden: true,
}

func init() {
	rootCmd.AddCommand(renderCmd)
}

func runRender(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true

	configPath := viper.GetString("config")

	data, err := config.LoadResolvedYAML(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	fmt.Fprint(os.Stdout, string(data))
	return nil
}
