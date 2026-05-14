package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dosquad/go-cliversion"
	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	exitCodeInterrupt = 130
)

var rootCmd = &cobra.Command{
	Use:           "mysql-reconciler",
	Short:         "mysql-reconciler - MySQL role reconciler",
	Long:          `mysql-reconciler is a tool for reconciling MySQL roles and permissions across MySQL servers.`,
	ValidArgs:     []string{},
	SilenceUsage:  false,
	SilenceErrors: true,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	_ = rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug output")
	_ = viper.BindPFlag("debug", rootCmd.PersistentFlags().Lookup("debug"))
	_ = viper.BindEnv("debug", "DEBUG")

	rootCmd.PersistentFlags().
		String("save-dir", "", "Directory for state store and migration history (overrides config state.dir)")
	_ = viper.BindPFlag("save-dir", rootCmd.PersistentFlags().Lookup("save-dir"))
	_ = viper.BindEnv("save-dir", "MYSQL_ROLE_RECONCILER_SAVE_DIR")

	rootCmd.PersistentFlags().StringP("config", "c", "", "Path to config.yaml (required)")
	_ = viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
	_ = viper.BindEnv("config", "MYSQL_ROLE_RECONCILER_CONFIG_FILE")
	// _ = rootCmd.MarkFlagRequired("config")

	rootCmd.Version = cliversion.Get().VersionString()
}

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// storageFromConfig creates a Storage from the config's state section,
// with --save-dir overriding state.dir when set.
func storageFromConfig(ctx context.Context, cfg *config.Config) (migrate.Storage, error) {
	sc := cfg.State

	// --save-dir flag overrides config state.dir for local storage
	if saveDir := viper.GetString("save-dir"); saveDir != "" {
		sc.Dir = saveDir
		if sc.Storage == "" {
			sc.Storage = "local"
		}
	}

	return migrate.NewStorage(ctx, migrate.StorageConfig{
		Type: sc.Storage,
		Dir:  sc.Dir,
		S3: migrate.S3Config{
			Bucket: sc.S3.Bucket,
			Prefix: sc.S3.Prefix,
			Region: sc.S3.Region,
		},
	})
}
