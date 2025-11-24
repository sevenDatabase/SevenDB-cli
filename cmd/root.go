package cmd

import (
	"fmt"
	"os"

	"github.com/sevendatabase/sevendb-cli/ironhawk"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dicedb-cli",
	Short: "Command line interface for DiceDB",
	Run: func(cmd *cobra.Command, args []string) {
		host, _ := cmd.Flags().GetString("host")
		port, _ := cmd.Flags().GetInt("port")
		// Build Ironhawk configuration from flags
		ackPolicyStr, _ := cmd.Flags().GetString("emit-ack-policy")
		autoReconnect, _ := cmd.Flags().GetBool("emitreconnect-on-reconnect")
		ackBatchSize, _ := cmd.Flags().GetInt("emit-ack-batch-size")
		ackFlushInterval, _ := cmd.Flags().GetDuration("emit-ack-flush-interval")
	verbose, _ := cmd.Flags().GetBool("verbose")
	watchDumpRaw, _ := cmd.Flags().GetBool("watch-dump-raw")
		noAck, _ := cmd.Flags().GetBool("no-ack")
		dedupeFile, _ := cmd.Flags().GetString("dedupe-state-file")
		reconnectRetries, _ := cmd.Flags().GetInt("reconnect-retries")
		reconnectBackoffMax, _ := cmd.Flags().GetDuration("reconnect-backoff-max")
		ackSeparateConn, _ := cmd.Flags().GetBool("ack-separate-conn")

		cfg := ironhawk.Config{
			AckPolicy:        ironhawk.ParseAckPolicy(ackPolicyStr),
			AutoReconnect:    autoReconnect,
			AckBatchSize:     ackBatchSize,
			AckFlushInterval: ackFlushInterval,
			Verbose:          verbose,
			NoAck:            noAck,
			DedupeStateFile:  dedupeFile,
			ReconnectRetries: reconnectRetries,
			ReconnectBackoffMax: reconnectBackoffMax,
            WatchDumpRaw:     watchDumpRaw,
            AckSeparateConn:  ackSeparateConn,
		}

		ironhawk.Run(host, port, cfg)
	},
}

func init() {
	rootCmd.PersistentFlags().String("host", "localhost", "hostname or ip address of the DiceDB server")
	rootCmd.PersistentFlags().Int("port", 7379, "port number of the DiceDB server")
	// Emission/ACK related flags (opt-in, non-breaking defaults)
	rootCmd.PersistentFlags().String("emit-ack-policy", "manual", "emission ack policy: auto-on-receive | auto-after-apply | manual")
	rootCmd.PersistentFlags().Bool("emitreconnect-on-reconnect", true, "automatically call EMITRECONNECT for active subscriptions on reconnect (best-effort)")
	rootCmd.PersistentFlags().Int("emit-ack-batch-size", 0, "batch up to N acks before sending (0=disable)")
	rootCmd.PersistentFlags().Duration("emit-ack-flush-interval", 0, "periodic flush interval for ack batching (0=disable)")
	rootCmd.PersistentFlags().Bool("verbose", false, "enable verbose logs for watch/ack/reconnect flows")
    rootCmd.PersistentFlags().Bool("watch-dump-raw", false, "dump every raw watch event line/result in addition to normal processing")
    // New emission resume/dedupe flags
    rootCmd.PersistentFlags().Bool("no-ack", false, "disable sending EMITACK for processed events")
    rootCmd.PersistentFlags().String("dedupe-state-file", "", "optional path to persist dedupe state (JSON)")
    rootCmd.PersistentFlags().Int("reconnect-retries", 0, "number of reconnect attempts before giving up (0=infinite)")
	rootCmd.PersistentFlags().Duration("reconnect-backoff-max", 16*1_000_000_000, "maximum reconnect backoff (e.g., 16s)")
	rootCmd.PersistentFlags().Bool("ack-separate-conn", false, "send EMITACK on a separate connection (default: false)")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
