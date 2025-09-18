package ironhawk

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/chzyer/readline"
	"github.com/dicedb/dicedb-go"
	"github.com/dicedb/dicedb-go/wire"
	"github.com/fatih/color"
	"github.com/google/shlex"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	boldRed   = color.New(color.FgRed, color.Bold).SprintFunc()
	boldBlue  = color.New(color.FgBlue, color.Bold).SprintFunc()
	boldGreen = color.New(color.FgGreen, color.Bold).SprintFunc()
)

func Run(host string, port int) {
	client, err := dicedb.NewClient(host, port)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	rl, err := readline.NewEx(&readline.Config{
		Prompt:      fmt.Sprintf("%s:%s> ", boldBlue(host), boldBlue(port)),
		HistoryFile: os.ExpandEnv("$HOME/.dicedb_history"),
	})
	if err != nil {
		fmt.Printf("%s failed to initialize readline: %v\n", boldRed("ERR"), err)
		return
	}
	defer rl.Close()

	// Signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	watchModeSignal := make(chan bool, 1)
	sigChanWatchMode := make(chan os.Signal, 1)

	// Handling interrupts in a goroutine
	go func() {
		for sig := range sigChan {
			select {
			// When in watch mode, capture the signal and send it to
			// the signal channel for watch mode
			case <-watchModeSignal:
				// Instead of exiting the REPL, send the signal to the
				// watch mode signal channel
				sigChanWatchMode <- sig
			default:
				// when not in watch mode, exit the REPL
				fmt.Println("\nreceived interrupt. exiting...")
				os.Exit(0)
			}
		}
	}()

	for {
		input, err := rl.Readline()
		if err != nil { // io.EOF, readline.ErrInterrupt
			break
		}
		input = strings.TrimSpace(input)

		if input == "exit" {
			return
		}

		if input == "" {
			continue
		}

		args := parseArgs(input)
		if len(args) == 0 {
			continue
		}

		c := &wire.Command{
			Cmd:  strings.ToUpper(args[0]),
			Args: args[1:],
		}

		resp := client.Fire(c)
		if resp.Status == wire.Status_ERR {
			renderResponse(resp)
			continue
		}

		if strings.HasSuffix(strings.ToUpper(args[0]), ".WATCH") {
			fmt.Println("entered the watch mode for", c.Cmd, strings.Join(c.Args, " "))

			// Send a signal to the primary Signal handler goroutine
			// that the watch mode has been entered
			watchModeSignal <- true

			// Get the watch channel and start watching for changes
			ch, err := client.WatchCh()
			if err != nil {
				fmt.Println("error watching:", err)
				continue
			}

			// Start watching for changes
			// until the user exits the watch mode
			shouldExitWatchMode := false
			for !shouldExitWatchMode {
				select {
				// If the user sends a signal Ctrl+C,
				// It is captured by the signal handler goroutine
				// and then sent to the watch mode signal channel
				// which will set the shouldExitWatchMode flag to true
				case <-sigChanWatchMode:
					fmt.Println("exiting the watch mode. back to command mode")
					shouldExitWatchMode = true
				case resp := <-ch:
					// If we get any response over the watch channel,
					// render the response
					renderResponse(resp)
				}
			}
		} else {
			// If the command is not a watch command, render the response
			// and continue to the next command in REPL
			renderResponse(resp)
		}
	}
}

func printZElement(e *wire.ZElement) {
	fmt.Printf("%d) %d, %s\n", e.Rank, e.Score, e.Member)
}

func printGEOElement(index int, e *wire.GEOElement) {
	fmt.Printf("%d) %d, %f, (%f, %f), %s\n", index, e.Hash, e.Distance, e.Coords.Longitude, e.Coords.Latitude, e.Member)
}

func renderResponse(resp *wire.Result) {
	if resp.Status == wire.Status_ERR {
		fmt.Printf("%s %s\n", boldRed("ERR"), resp.Message)
		return
	}

	fmt.Printf("%s ", boldGreen(resp.Message))
	if resp.Fingerprint64 != 0 {
		fmt.Printf("[fingerprint=%d] ", resp.Fingerprint64)
	}

	switch resp.Response.(type) {
	case *wire.Result_GETRes:
		fmt.Printf("\"%s\"\n", resp.GetGETRes().Value)
	case *wire.Result_GETDELRes:
		fmt.Printf("\"%s\"\n", resp.GetGETDELRes().Value)
	case *wire.Result_SETRes:
		fmt.Printf("\n")
	case *wire.Result_FLUSHDBRes:
		fmt.Printf("\n")
	case *wire.Result_DELRes:
		fmt.Printf("%d\n", resp.GetDELRes().Count)
	case *wire.Result_DECRRes:
		fmt.Printf("%d\n", resp.GetDECRRes().Value)
	case *wire.Result_INCRRes:
		fmt.Printf("%d\n", resp.GetINCRRes().Value)
	case *wire.Result_DECRBYRes:
		fmt.Printf("%d\n", resp.GetDECRBYRes().Value)
	case *wire.Result_INCRBYRes:
		fmt.Printf("%d\n", resp.GetINCRBYRes().Value)
	case *wire.Result_ECHORes:
		fmt.Printf("%s\n", resp.GetECHORes().Message)
	case *wire.Result_EXISTSRes:
		fmt.Printf("%d\n", resp.GetEXISTSRes().Count)
	case *wire.Result_EXPIRERes:
		fmt.Printf("%v\n", resp.GetEXPIRERes().IsChanged)
	case *wire.Result_EXPIREATRes:
		fmt.Printf("%v\n", resp.GetEXPIREATRes().IsChanged)
	case *wire.Result_EXPIRETIMERes:
		fmt.Printf("%d\n", resp.GetEXPIRETIMERes().UnixSec)
	case *wire.Result_TTLRes:
		fmt.Printf("%d\n", resp.GetTTLRes().Seconds)
	case *wire.Result_GETEXRes:
		fmt.Printf("\"%s\"\n", resp.GetGETEXRes().Value)
	case *wire.Result_GETSETRes:
		fmt.Printf("\"%s\"\n", resp.GetGETSETRes().Value)
	case *wire.Result_HANDSHAKERes:
		fmt.Printf("\n")
	case *wire.Result_HGETRes:
		fmt.Printf("\"%s\"\n", resp.GetHGETRes().Value)
	case *wire.Result_HSETRes:
		fmt.Printf("%d\n", resp.GetHSETRes().Count)
	case *wire.Result_HGETALLRes:
		fmt.Printf("\n")
		for i, e := range resp.GetHGETALLRes().Elements {
			fmt.Printf("%d) %s=\"%s\"\n", i, e.Key, e.Value)
		}
	case *wire.Result_KEYSRes:
		fmt.Printf("\n")
		for i, key := range resp.GetKEYSRes().Keys {
			fmt.Printf("%d) %s\n", i, key)
		}
	case *wire.Result_PINGRes:
		fmt.Printf("\"%s\"\n", resp.GetPINGRes().Message)
	case *wire.Result_TYPERes:
		fmt.Printf("%s\n", resp.GetTYPERes().Type)
	case *wire.Result_ZADDRes:
		fmt.Printf("%d\n", resp.GetZADDRes().Count)
	case *wire.Result_ZCOUNTRes:
		fmt.Printf("%d\n", resp.GetZCOUNTRes().Count)
	case *wire.Result_ZRANGERes:
		fmt.Printf("\n")
		for _, e := range resp.GetZRANGERes().Elements {
			printZElement(e)
		}
	case *wire.Result_ZPOPMAXRes:
		fmt.Printf("\n")
		for _, e := range resp.GetZPOPMAXRes().Elements {
			printZElement(e)
		}
	case *wire.Result_ZPOPMINRes:
		fmt.Printf("\n")
		for _, e := range resp.GetZPOPMINRes().Elements {
			printZElement(e)
		}
	case *wire.Result_ZREMRes:
		fmt.Printf("%d\n", resp.GetZREMRes().Count)
	case *wire.Result_ZCARDRes:
		fmt.Printf("%d\n", resp.GetZCARDRes().Count)
	case *wire.Result_ZRANKRes:
		printZElement(resp.GetZRANKRes().Element)
	case *wire.Result_GETWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_HGETWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_HGETALLWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_ZRANGEWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_ZCARDWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_ZCOUNTWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_ZRANKWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_UNWATCHRes:
		fmt.Printf("\n")
	case *wire.Result_GEOADDRes:
		fmt.Printf("%d\n", resp.GetGEOADDRes().Count)
	case *wire.Result_GEODISTRes:
		fmt.Printf("%f\n", resp.GetGEODISTRes().Distance)
	case *wire.Result_GEOSEARCHRes:
		fmt.Printf("\n")
		for i, e := range resp.GetGEOSEARCHRes().Elements {
			printGEOElement(i, e)
		}
	case *wire.Result_GEOHASHRes:
		fmt.Printf("\n")
		for i, hash := range resp.GetGEOHASHRes().Hashes {
			if len(hash) == 0 {
				hash = "nil"
			}
			fmt.Printf("%d) %s\n", i, hash)
		}
	case *wire.Result_GEOPOSRes:
		fmt.Printf("\n")
		for i, coord := range resp.GetGEOPOSRes().Coords {
			if coord.Latitude == 0 || coord.Longitude == 0 {
				fmt.Printf("%d) (nil)\n", i)
			} else {
				fmt.Printf("%d) %f, %f\n", i, coord.Longitude, coord.Latitude)
			}
		}
	default:
		fmt.Println("note: this response is JSON serialized version of the response because it is not supported by this version of the CLI. You can upgrade the CLI to the latest version to get a formatted response.")
		b, err := protojson.Marshal(resp)
		if err != nil {
			log.Fatalf("failed to marshal to JSON: %v", err)
		}

		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)

		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	}
}

func parseArgs(input string) []string {
	args, err := shlex.Split(input)
	if err != nil {
		fmt.Printf("%s failed to parse command: %v\n", boldRed("ERR"), err)
		return []string{}
	}
	return args
}
