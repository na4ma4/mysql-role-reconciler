package main

import (
	"context"
	"fmt"
	"os"

	"github.com/na4ma4/mysql-role-reconciler/internal/config"
	"github.com/na4ma4/mysql-role-reconciler/internal/migrate"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var historyCmd = &cobra.Command{
	Use:   "history -c config.yaml",
	Short: "Show migration history",
	Long:  `Display previously applied migrations from the history directory.`,
	RunE:  runHistory,
}

func init() {
	historyCmd.Flags().Bool("last", false, "Show only the most recent migration entry")

	rootCmd.AddCommand(historyCmd)
}

func runHistory(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true

	showLast, _ := cmd.Flags().GetBool("last")

	var cfg *config.Config
	{
		var err error
		cfg, _, _, err = config.Load(viper.GetString("config"))
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
	}

	ctx := context.Background()

	store, err := storageFromConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("creating storage: %w", err)
	}

	entries, err := migrate.ReadHistory(ctx, store)
	if err != nil {
		return fmt.Errorf("reading history: %w", err)
	}

	if len(entries) == 0 {
		fmt.Fprintln(os.Stdout, "No migration history found.")
		return nil
	}

	if showLast {
		entries = entries[len(entries)-1:]
	}

	for i, entry := range entries {
		fmt.Fprintf(os.Stdout, "Entry #%d\n", i+1)
		if entry.Version == "" {
			fmt.Fprintf(os.Stdout, "  Version:    (missing version, unable to parse entry)\n")
		} else {
			fmt.Fprintf(os.Stdout, "  Version:    %s\n", entry.Version)
		}
		fmt.Fprintf(os.Stdout, "  Timestamp:   %s\n", entry.Timestamp)
		fmt.Fprintf(os.Stdout, "  Environment: %s\n", entry.Environment)
		fmt.Fprintf(os.Stdout, "  Server:      %s\n", entry.Server)
		fmt.Fprintf(os.Stdout, "  Checksum:    %s (%s)\n", entry.Checksum, isChecksumValid(entry))
		fmt.Fprintf(os.Stdout, "  Statements (%d):\n", len(entry.Statements))
		for _, s := range entry.Statements {
			fmt.Fprintf(os.Stdout, "    %s\n", s)
		}
		fmt.Fprintln(os.Stdout)
	}

	return nil
}

func isChecksumValid(entry migrate.HistoryEntry) string {
	if entry.Checksum == "" {
		return "N/A"
	}
	if entry.Version == "" {
		return "unknown (missing version)"
	}
	if entry.Version == migrate.Version1 {
		if entry.ValidateChecksum() {
			return "valid"
		}

		return "invalid"
	}

	return "unknown (unrecognized version)"
}
