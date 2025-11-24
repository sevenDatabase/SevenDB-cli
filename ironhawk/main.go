package ironhawk

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"
	"strconv"
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

func Run(host string, port int, cfg Config) {
	client, err := dicedb.NewClient(host, port)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	mgr := NewManager(cfg)
	// Set up dedupe store
	var ds DedupeStore
	if cfg.DedupeStateFile != "" {
		if fds, err := NewFileDedupe(cfg.DedupeStateFile); err == nil {
			ds = fds
		} else {
			fmt.Printf("%s failed to open dedupe state file (%v); using in-memory dedupe only\n", boldRed("WARN"), err)
			ds = NewInMemoryDedupe()
		}
	} else {
		ds = NewInMemoryDedupe()
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:      fmt.Sprintf("%s:%s> ", boldBlue(host), boldBlue(port)),
		HistoryFile: os.ExpandEnv("$HOME/.dicedb_history"),
	})
	if err != nil {
		fmt.Printf("%s failed to initialize readline: %v\n", boldRed("ERR"), err)
		return
	}
	defer rl.Close()

	// Probe server for a session-scoped clientID via HELLO (if supported)
	// This helps construct SubID as clientID:fingerprint64 expected by server.
	if cfg.Verbose {
		fmt.Println("probing server for clientID via HELLO…")
	}
	helloRes := client.Fire(&wire.Command{Cmd: "HELLO"})
	if helloRes != nil && helloRes.Status != wire.Status_ERR {
		if id, ok := extractClientID(helloRes); ok {
			mgr.SetClientID(id)
			if cfg.Verbose {
				fmt.Println("clientID:", id)
			}
		} else if cfg.Verbose {
			fmt.Println("HELLO responded but clientID not found in payload")
		}
	} else if cfg.Verbose {
		fmt.Println("HELLO not supported or failed; falling back to fingerprint-only SubID")
	}

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

		// Local helper commands (prefixed with ":")
		// :emitreconnect -> uses current subscription and last processed index
		if strings.HasPrefix(input, ":") {
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(input, ":")))
			if len(fields) == 0 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "emitreconnect":
				sub := mgr.Current()
				if sub == nil {
					fmt.Println(boldRed("ERR"), "no active subscription for reconnect")
					continue
				}
				status, nextIdx, err := emitReconnect(client, sub.Key, sub.SubID, sub.LastProcessedCommitIndex)
				if err != nil {
					fmt.Println(boldRed("ERR"), err)
					continue
				}
				switch status {
				case "OK":
					if cfg.Verbose {
						fmt.Printf("resume allowed: nextIndex=%d\n", nextIdx)
					}
					sub.mu.Lock()
					sub.ResumeNextIndex = nextIdx
					sub.State = StateResuming
					// Defensive: align watermark
					if nextIdx > 0 && nextIdx > sub.LastProcessedCommitIndex+1 {
						sub.LastProcessedCommitIndex = nextIdx - 1
					}
					// Reset any pending batch state to keep ACKs monotonic after resume
					sub.highestIndex = 0
					sub.pendingCount = 0
					sub.mu.Unlock()
					fmt.Println(boldGreen("OK"), nextIdx)
				case "STALE_SEQUENCE", "INVALID_SEQUENCE", "SUBSCRIPTION_NOT_FOUND":
					sub.mu.Lock()
					sub.State = StatePaused
					sub.mu.Unlock()
					fmt.Println(boldRed("WARN"), status, "-> please re-subscribe (issue the WATCH command again)")
				default:
					fmt.Println(status)
				}
				continue
			case "emitack":
				// :emitack [commitIndex]
				sub := mgr.Current()
				if sub == nil {
					fmt.Println(boldRed("ERR"), "no active subscription for ack")
					continue
				}
				var idx uint64
				if len(fields) >= 2 {
					if u, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						idx = u
					}
				}
				if idx == 0 {
					sub.mu.Lock()
					idx = sub.LastProcessedCommitIndex
					sub.mu.Unlock()
				}
				if idx == 0 {
					fmt.Println(boldRed("ERR"), "no commit index available to ack")
					continue
				}
				if err := sendEmitAck(client, sub.Key, sub.SubID, idx); err != nil {
					fmt.Println(boldRed("ERR"), err)
				} else {
					if cfg.Verbose {
						fmt.Println("ACK sent for index", idx)
					}
				}
				continue
			}
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

			// Refresh ClientID right before WATCH to ensure we have the correct ID for this connection
			// This helps if the connection was reset or if we are in a pool (though CLI usually uses one)
			helloRes := client.Fire(&wire.Command{Cmd: "HELLO"})
			if helloRes != nil && helloRes.Status != wire.Status_ERR {
				if id, ok := extractClientID(helloRes); ok {
					mgr.SetClientID(id)
					if cfg.Verbose {
						fmt.Println("refreshed clientID:", id)
					}
				}
			}

			resp := client.Fire(c)
			if resp.Status == wire.Status_ERR {
				renderResponse(resp)
				continue
			}

			// Use fingerprint as best-effort SubID if server doesn't provide a distinct ID
			// Build SubID as clientID:fingerprint64 when clientID is available
			fingerprint := resp.Fingerprint64
			// Fallback: try to parse fingerprint from message if not in typed field
			if fingerprint == 0 && resp.Message != "" {
				if fp, ok := parseFingerprintFromMsg(resp.Message); ok {
					fingerprint = fp
					if cfg.Verbose {
						fmt.Println("parsed fingerprint from message:", fingerprint)
					}
				}
			}

			subID := fmt.Sprintf("%d", fingerprint)
			if cid := mgr.ClientID(); cid != "" {
				subID = fmt.Sprintf("%s:%s", cid, subID)
			} else if cfg.Verbose {
				fmt.Println("warning: clientID unknown; SubID will be fingerprint-only—server may not accept ACK/RECONNECT")
			}
			fmt.Println("SubID:", subID) // Explicitly print SubID for debugging

			key := ""
			if len(c.Args) > 0 {
				key = c.Args[0]
			}
			sub := &Subscription{
				SubID:      subID,
				Key:        key,
				Fingerprint: fingerprint,
				AckPolicy:  mgr.cfg.AckPolicy,
				State:      StateActive,
				WatchCmd:   c.Cmd,
				WatchArgs:  append([]string(nil), c.Args...),
			}
			mgr.SetCurrent(sub)

			// Always create a dedicated ACK client to avoid deadlocks on the watch connection
			var ackClient *dicedb.Client
			if ac, err := dicedb.NewClient(host, port); err == nil {
				ackClient = ac
				if cfg.Verbose { fmt.Println("ack client established for EMITACK") }
			} else {
				fmt.Println(boldRed("ERR"), "failed to create ack client; ACKs may fail or deadlock:", err)
			}

			// Get the watch channel and start watching for changes
			ch, err := client.WatchCh()
			if err != nil {
				fmt.Println("error watching:", err)
				if ackClient != nil { ackClient.Close() }
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
				case resp, ok := <-ch:
					if !ok || resp == nil {
						// Disconnected: attempt to reconnect if enabled
						if !cfg.AutoReconnect {
							fmt.Println(boldRed("ERR"), "connection lost; auto reconnect disabled. Exiting watch mode.")
							shouldExitWatchMode = true
							break
						}
						if cfg.Verbose {
							fmt.Println("connection lost; attempting to reconnect…")
						}
						// retry loop with simple backoff
						backoff := time.Second
						maxBackoff := cfg.ReconnectBackoffMax
						if maxBackoff <= 0 {
							maxBackoff = 16 * time.Second
						}
						retries := 0
						maxRetries := cfg.ReconnectRetries
						var newClient *dicedb.Client
						for {
							cl, err := dicedb.NewClient(host, port)
							if err == nil {
								newClient = cl
								break
							}
							if cfg.Verbose {
								fmt.Println("reconnect failed:", err, "; retrying in", backoff)
							}
							time.Sleep(backoff)
							if backoff < maxBackoff { backoff *= 2; if backoff > maxBackoff { backoff = maxBackoff } }
							retries++
							if maxRetries > 0 && retries >= maxRetries {
								fmt.Println(boldRed("ERR"), "reconnect retries exhausted. Exiting watch mode.")
								shouldExitWatchMode = true
								break
							}
						}
						if shouldExitWatchMode { break }

						// swap client and re-probe HELLO for new clientID
						client = newClient
						helloRes := client.Fire(&wire.Command{Cmd: "HELLO"})
						if helloRes != nil && helloRes.Status != wire.Status_ERR {
							if id, ok := extractClientID(helloRes); ok {
								mgr.SetClientID(id)
								if cfg.Verbose {
									fmt.Println("clientID:", id)
								}
							}
						}


						// Try EMITRECONNECT first
						if cfg.Verbose {
							fmt.Println("calling EMITRECONNECT with last index", sub.LastProcessedCommitIndex)
						}
						// Compute last processed index from dedupe store for current epoch, if known
						lastIdx := sub.LastProcessedCommitIndex
						if l, ok := ds.Get(sub.Fingerprint, sub.EpochUUID, sub.EpochCounter); ok {
							lastIdx = l
						}
						// IMPORTANT: EMITRECONNECT must be called with the OLD sub_id (oldClientID:fp)
						oldSubID := sub.SubID
						if cfg.Verbose {
							fmt.Println("EMITRECONNECT using old sub_id:", oldSubID)
						}
						status, nextIdx, err := emitReconnect(client, sub.Key, oldSubID, lastIdx)
						if err == nil && strings.ToUpper(status) == "OK" {
							if cfg.Verbose {
								fmt.Printf("resume allowed: nextIndex=%d\n", nextIdx)
							}
							sub.mu.Lock()
							sub.ResumeNextIndex = nextIdx
							sub.State = StateResuming
							if nextIdx > 0 && nextIdx > sub.LastProcessedCommitIndex+1 {
								sub.LastProcessedCommitIndex = nextIdx - 1
							}
							// reset batch
							sub.highestIndex = 0
							sub.pendingCount = 0
							// After OK, switch to new sub_id = newClientID:fp for subsequent ACKs
							if cid := mgr.ClientID(); cid != "" {
								newSubID := fmt.Sprintf("%s:%d", cid, sub.Fingerprint)
								if cfg.Verbose {
									fmt.Println("switching to new sub_id:", newSubID)
								}
								sub.SubID = newSubID
							}
							sub.mu.Unlock()
							// re-open watch channel
							ch, _ = client.WatchCh()
							// Recreate ack client to keep separation
							if ackClient != nil { ackClient.Close(); ackClient = nil }
							if ac, err := dicedb.NewClient(host, port); err == nil { ackClient = ac }
							continue
						}

						// If reconnect isn’t valid, re-subscribe using the original WATCH command
						if cfg.Verbose {
							fmt.Println("EMITRECONNECT not accepted (", status, ") — re-subscribing…")
						}
						watchCmd := &wire.Command{Cmd: sub.WatchCmd, Args: append([]string(nil), sub.WatchArgs...)}
						wresp := client.Fire(watchCmd)
						if wresp == nil || wresp.Status == wire.Status_ERR {
							if cfg.Verbose {
								fmt.Println("re-subscribe failed; will keep retrying via channel close path…")
							}
							// sleep a moment and loop back to try again
							time.Sleep(time.Second)
							continue
						}
						// update fingerprint and SubID
						sub.mu.Lock()
						sub.Fingerprint = wresp.Fingerprint64
						// compute sub_id from current clientID and new fingerprint
						if cid2 := mgr.ClientID(); cid2 != "" {
							sub.SubID = fmt.Sprintf("%s:%d", cid2, sub.Fingerprint)
						} else {
							sub.SubID = fmt.Sprintf("%d", sub.Fingerprint)
						}
						sub.State = StateActive
						sub.mu.Unlock()
						ch, _ = client.WatchCh()
						// Refresh ack client as well to avoid using a stale connection
						if ackClient != nil { ackClient.Close(); ackClient = nil }
						if ac, err := dicedb.NewClient(host, port); err == nil { ackClient = ac }
						continue
					}
					// If we get any response over the watch channel, handle emission prefix
					if cfg.Verbose {
						fmt.Printf("[watch recv] status=%v message=%q hasResponse=%t\n", resp.Status, resp.Message, resp.Response != nil)
					}
					// Always dump raw event when enabled (for debugging), but continue normal processing
					if cfg.WatchDumpRaw {
						if strings.TrimSpace(resp.Message) != "" {
							fmt.Println("RAW:", resp.Message)
						} else {
							b, err := protojson.Marshal(resp)
							if err == nil {
								var m map[string]interface{}
								_ = json.Unmarshal(b, &m)
								nb, _ := json.MarshalIndent(m, "", "  ")
								fmt.Println("RAW JSON:")
								fmt.Println(string(nb))
							} else {
								fmt.Println("RAW (marshal error):", err)
							}
						}
					}
					eu, ec, ci, tail, okp := parseEmitPrefix(resp.Message)
					// If prefix was present but commit index missing/invalid, print raw line to aid visibility
					if okp && ci == 0 {
						fmt.Println(resp.Message)
						continue
					}
					if okp && ci > 0 {
						// Dedupe by (fp, epochUUID, epochCounter)
						fp := sub.Fingerprint
						if shouldProcess(fp, eu, ec, ci, ds) {
							// Update subscription state
							sub.mu.Lock()
							sub.LastProcessedCommitIndex = ci
							sub.EpochUUID = eu
							sub.EpochCounter = ec
							sub.mu.Unlock()
							// Transparency: print parsed epoch and index
							fmt.Printf("[epoch=%s:%d, commit_index=%d] ", eu, ec, ci)
							if cfg.Verbose {
								if last, ok := ds.Get(fp, eu, ec); ok {
									fmt.Printf("(last=%d) ", last)
								}
							}
							// Print payload tail or pretty-print typed payload when tail is empty
							if strings.TrimSpace(tail) != "" {
								fmt.Println(tail)
							} else if resp.Response != nil {
								b, err := protojson.Marshal(resp)
								if err == nil {
									var m map[string]interface{}
									_ = json.Unmarshal(b, &m)
									nb, _ := json.MarshalIndent(m, "", "  ")
									fmt.Println()
									fmt.Println(string(nb))
								} else {
									fmt.Println()
								}
							} else {
								fmt.Println()
							}
							// ACK synchronously (default behavior) to keep ordering and avoid races
							if !cfg.NoAck {
								var ackFC fireClient = client
								if ackClient != nil { ackFC = ackClient }
								if err := sendEmitAck(ackFC, sub.Key, sub.SubID, ci); err != nil {
									if cfg.Verbose {
										fmt.Println(boldRed("ERR"), "ACK failed:", err)
									}
								} else if cfg.Verbose {
									fmt.Printf("[ack sent idx=%d]\n", ci)
								}
							}
						} else {
							if cfg.Verbose {
								if last, ok := ds.Get(fp, eu, ec); ok {
									fmt.Printf("[skip duplicate epoch=%s:%d idx=%d last=%d]\n", eu, ec, ci, last)
								} else {
									fmt.Printf("[skip duplicate epoch=%s:%d idx=%d]\n", eu, ec, ci)
								}
							}
						}
						// Skip default renderer for emission lines
						continue
					}
					// Fallback when no emit prefix
					if strings.TrimSpace(resp.Message) != "" {
						// Show raw message directly for maximum visibility
						fmt.Println(resp.Message)
						// Apply ack policy if enabled (legacy path)
						var ackFC fireClient = client
						if ackClient != nil { ackFC = ackClient }
						maybeAckOnReceive(mgr, ackFC, sub, resp)
						continue
					}
					// If message is empty, try the generic renderer (typed payloads)
					renderResponse(resp)
					// Apply ack policy if enabled
					var ackFC fireClient = client
					if ackClient != nil { ackFC = ackClient }
					maybeAckOnReceive(mgr, ackFC, sub, resp)
				}
			}

			// Stop any batching ticker and flush pending highest index on exit
			sub.mu.Lock()
			if sub.flushTicker != nil {
				sub.flushTicker.Stop()
			}
			hi := sub.highestIndex
			pc := sub.pendingCount
			sub.highestIndex = 0
			sub.pendingCount = 0
			sub.mu.Unlock()
			if pc > 0 && !cfg.NoAck {
				var ackFC fireClient = client
				if ackClient != nil { ackFC = ackClient }
				// final flush: send synchronously
				_ = sendEmitAck(ackFC, sub.Key, sub.SubID, hi)
			}

			// Send UNWATCH to server to clean up subscription
			// Use the original fingerprint, not the sub_id
			if ackClient != nil {
				unwatchCmd := &wire.Command{
					Cmd:  "GET.WATCH.UNWATCH",
					Args: []string{fmt.Sprintf("%d", sub.Fingerprint)},
				}
				if cfg.Verbose {
					fmt.Println("sending UNWATCH for fingerprint:", sub.Fingerprint)
				}
				ackClient.Fire(unwatchCmd)
			}

			if ackClient != nil { ackClient.Close() }
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
	if resp == nil {
		// Defensive guard: nothing to render (likely watch channel closed)
		return
	}
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
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_HGETWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_HGETALLWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_ZRANGEWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_ZCARDWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_ZCOUNTWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_ZRANKWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
	case *wire.Result_UNWATCHRes:
		b, err := protojson.Marshal(resp)
		if err != nil { log.Fatalf("failed to marshal to JSON: %v", err) }
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		nb, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(nb))
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
	if err == nil {
		return args
	}
	// Fallback: tolerant splitter that handles basic quoting and
	// recovers from unterminated quotes by taking the rest of line.
	// This is intentionally simple to cover REPL convenience cases only.
	var out []string
	var cur []rune
	inQuote := false
	var q rune
	rs := []rune(input)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		if inQuote {
			if ch == '\\' { // escape next char inside quotes
				if i+1 < len(rs) {
					cur = append(cur, rs[i+1])
					i++
				} else {
					// trailing backslash, keep it
					cur = append(cur, ch)
				}
				continue
			}
			if ch == q { // closing quote
				inQuote = false
				continue
			}
			cur = append(cur, ch)
			continue
		}
		// outside quotes
		switch ch {
		case '\'', '"':
			inQuote = true
			q = ch
		case ' ', '\t', '\n', '\r':
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
		default:
			cur = append(cur, ch)
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// parseFingerprintFromMsg attempts to extract [fingerprint=123] from a message string
func parseFingerprintFromMsg(msg string) (uint64, bool) {
	// Simple scan for "[fingerprint="
	start := strings.Index(msg, "[fingerprint=")
	if start == -1 {
		return 0, false
	}
	rest := msg[start+len("[fingerprint="):]
	end := strings.Index(rest, "]")
	if end == -1 {
		return 0, false
	}
	valStr := rest[:end]
	val, err := strconv.ParseUint(valStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return val, true
}
