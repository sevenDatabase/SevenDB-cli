# DiceDB CLI

This is a command line interface for [SevenDB](https://sevendb.com).

## Get Started

### Using cURL

The best way to connect to DiceDB is using [SevenDB CLI](https://github.com/sevenDatabase/SevenDB-cli) and you can install it by running the following command

```bash
$ sudo su
$ curl -sL https://raw.githubusercontent.com/sevenDatabase/SevenDB-cli/refs/heads/master/install.sh | sh
```

If you are working on unsupported OS (as per above script), you can always follow the installation instructions mentioned in the [dicedb/cli](https://github.com/dicedb/dicedb-cli) repository.

### Building from source

```sh
$ git clone https://github.com/sevenDatabase/SevenDB-cli
$ cd sevenDB-cli
$ make build
```

The above command will create a binary `sevendb-cli`. Execute the binary will
start the CLI and will try to connect to the DiceDB server.

## Usage

Run the executable to start the interactive prompt (REPL)

```bash
$ sevendb-cli
```

You should see

```sh
localhost:7379>
```

To connect to some other host or port, you can pass the flags `--host` and `--port` with apt parameters.
You can also get all available parameters by firing

```sh
$ sevendb-cli --help
```

## Firing commands

You can execute any DiceDB command directly:

```bash
localhost:7379> SET k1 v1
OK OK
localhost:7379> GET k1
OK "v1"
localhost:7379> DEL k1
OK 1
```

You can find all available commands at [dicedb.io/docs](https://dicedb.io/docs).

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
