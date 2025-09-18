package cmd

import (
	"fmt"
	"os"

	"github.com/dicedb/dicedb-cli/ironhawk"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dicedb-cli",
	Short: "Command line interface for DiceDB",
	Run: func(cmd *cobra.Command, args []string) {
		host, _ := cmd.Flags().GetString("host")
		port, _ := cmd.Flags().GetInt("port")
		ironhawk.Run(host, port)
	},
}

func init() {
	rootCmd.PersistentFlags().String("host", "localhost", "hostname or ip address of the DiceDB server")
	rootCmd.PersistentFlags().Int("port", 7379, "port number of the DiceDB server")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
