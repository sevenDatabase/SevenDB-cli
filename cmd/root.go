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

		cfg := ironhawk.Config{
			AckPolicy:        ironhawk.ParseAckPolicy(ackPolicyStr),
			AutoReconnect:    autoReconnect,
			AckBatchSize:     ackBatchSize,
			AckFlushInterval: ackFlushInterval,
			Verbose:          verbose,
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
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
