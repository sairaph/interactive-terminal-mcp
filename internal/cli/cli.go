// Package cli parses arguments and runs the one-shot commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/sairaph/interactive-terminal-mcp/internal/bootstrap"
)

// Options carries the process-level inputs a command may need.
type Options struct {
	Version    string
	Executable string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// Command is a parsed invocation.
type Command struct {
	Name string

	Session string
	Text    string
	Keys    string
	Command string
	Cwd     string
	Name2   string
	Signal  string
	Lines   int
	Cols    int
	Rows    int
	Wait    float64
	WaitSet bool
	NoEnter bool

	Detach  bool
	Stop    bool
	Kill    bool
	All     bool
	Clients []string
	Yes     bool
}

// ErrUsage marks a parse failure so the caller can print usage and exit 2.
var ErrUsage = errors.New("usage")

// Parse converts arguments into a command.
func Parse(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{Name: "app"}, nil
	}
	command := Command{Name: args[0]}
	rest := args[1:]

	switch command.Name {
	case "help", "-h", "--help":
		command.Name = "help"
		return command, nil
	case "version", "-v", "--version":
		command.Name = "version"
		return command, nil
	case "mcp":
		if len(rest) > 0 {
			return Command{}, fmt.Errorf("%w: mcp takes no arguments", ErrUsage)
		}
		return command, nil
	case "daemon":
		for index := 0; index < len(rest); index++ {
			switch rest[index] {
			case "--detach":
				command.Detach = true
			case "--stop":
				command.Stop = true
			case "--kill-sessions":
				command.Kill = true
			default:
				return Command{}, fmt.Errorf("%w: unknown daemon flag %q", ErrUsage, rest[index])
			}
		}
		return command, nil
	case "doctor", "status", "config":
		return command, nil
	case "configure", "install", "uninstall":
		for index := 0; index < len(rest); index++ {
			switch rest[index] {
			case "--all":
				command.All = true
			case "--yes", "-y":
				command.Yes = true
			case "--client":
				value, next, err := takeValue(rest, index, "--client")
				if err != nil {
					return Command{}, err
				}
				command.Clients = append(command.Clients, strings.Split(value, ",")...)
				index = next
			default:
				return Command{}, fmt.Errorf("%w: unknown %s flag %q", ErrUsage, command.Name, rest[index])
			}
		}
		return command, nil
	case "ls", "list":
		command.Name = "ls"
		return command, nil
	case "new":
		return parseNew(command, rest)
	case "read", "attach", "kill", "tail", "head", "send", "rename":
		return parseSessionCommand(command, rest)
	default:
		return Command{}, fmt.Errorf("%w: unknown command %q", ErrUsage, command.Name)
	}
}

func parseNew(command Command, rest []string) (Command, error) {
	for index := 0; index < len(rest); index++ {
		argument := rest[index]
		switch {
		case argument == "--cmd" || argument == "--command":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Command, index = value, next
		case argument == "--cwd":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Cwd, index = value, next
		case argument == "--cols":
			value, next, err := takeInt(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Cols, index = value, next
		case argument == "--rows":
			value, next, err := takeInt(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Rows, index = value, next
		case argument == "--wait":
			value, next, err := takeFloat(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Wait, command.WaitSet, index = value, true, next
		case strings.HasPrefix(argument, "-"):
			return Command{}, fmt.Errorf("%w: unknown new flag %q", ErrUsage, argument)
		default:
			if command.Name2 != "" {
				return Command{}, fmt.Errorf("%w: new takes at most one name", ErrUsage)
			}
			command.Name2 = argument
		}
	}
	return command, nil
}

func parseSessionCommand(command Command, rest []string) (Command, error) {
	for index := 0; index < len(rest); index++ {
		argument := rest[index]
		switch {
		case argument == "--text":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Text, index = value, next
		case argument == "--keys":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Keys, index = value, next
		case argument == "--no-enter":
			command.NoEnter = true
		case argument == "--signal":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Signal, index = value, next
		case argument == "-n" || argument == "--lines":
			value, next, err := takeInt(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Lines, index = value, next
		case argument == "--wait":
			value, next, err := takeFloat(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Wait, command.WaitSet, index = value, true, next
		case argument == "--name":
			value, next, err := takeValue(rest, index, argument)
			if err != nil {
				return Command{}, err
			}
			command.Name2, index = value, next
		case strings.HasPrefix(argument, "-"):
			return Command{}, fmt.Errorf("%w: unknown %s flag %q", ErrUsage, command.Name, argument)
		default:
			if command.Session != "" {
				if command.Name == "rename" && command.Name2 == "" {
					command.Name2 = argument
					continue
				}
				return Command{}, fmt.Errorf("%w: %s takes one session", ErrUsage, command.Name)
			}
			command.Session = argument
		}
	}
	if command.Name == "kill" && command.Session == "" {
		return Command{}, fmt.Errorf("%w: kill requires a session; it is never inferred", ErrUsage)
	}
	if command.Name == "attach" && command.Session == "" {
		return Command{}, fmt.Errorf("%w: attach requires a session", ErrUsage)
	}
	if command.Name == "rename" && (command.Session == "" || command.Name2 == "") {
		return Command{}, fmt.Errorf("%w: usage: interactive-terminal-mcp rename <session> <new-name>", ErrUsage)
	}
	if command.Name == "send" && command.Text == "" && command.Keys == "" {
		return Command{}, fmt.Errorf("%w: send requires --text or --keys", ErrUsage)
	}
	return command, nil
}

func takeValue(args []string, index int, flag string) (string, int, error) {
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("%w: %s needs a value", ErrUsage, flag)
	}
	return args[index+1], index + 1, nil
}

func takeInt(args []string, index int, flag string) (int, int, error) {
	raw, next, err := takeValue(args, index, flag)
	if err != nil {
		return 0, index, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, index, fmt.Errorf("%w: %s needs a whole number, got %q", ErrUsage, flag, raw)
	}
	return value, next, nil
}

func takeFloat(args []string, index int, flag string) (float64, int, error) {
	raw, next, err := takeValue(args, index, flag)
	if err != nil {
		return 0, index, err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, index, fmt.Errorf("%w: %s needs a number, got %q", ErrUsage, flag, raw)
	}
	return value, next, nil
}

// Usage is the help text.
const Usage = `interactive-terminal-mcp - persistent terminal sessions for AI agents

Usage:
  interactive-terminal-mcp                  open the interactive application
  interactive-terminal-mcp mcp              run the MCP server on stdio
  interactive-terminal-mcp configure        choose AI clients and settings

Sessions:
  ls                                        list sessions
  new [name] [--cmd C] [--cwd D]            create a session
      [--cols N] [--rows N] [--wait S]
  attach <session>                          open a session in this terminal
  read [session] [--wait S]                 print a session's screen
  send [session] --text T | --keys K        type into a session
       [--no-enter] [--wait S]
  tail [session] [-n N]                     print recent output
  head [session] [-n N]                     print earliest output
  rename <session> <new-name>               rename a session
  kill <session> [--signal TERM|INT|HUP|KILL]

Daemon:
  daemon [--detach]                         run the session daemon
  daemon --stop [--kill-sessions]           stop it
  status                                    show daemon status
  doctor                                    diagnose the installation

Other:
  config [path]                             show settings
  version, help

Session logs live under ~/.interactive-terminal-mcp/sessions.
`

// Run executes a parsed command.
func Run(ctx context.Context, runtime *bootstrap.Runtime, command Command, options Options) int {
	if options.Stdout == nil {
		options.Stdout = os.Stdout
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}

	switch command.Name {
	case "help":
		fmt.Fprint(options.Stdout, Usage)
		return 0
	case "version":
		fmt.Fprintf(options.Stdout, "interactive-terminal-mcp %s\n", options.Version)
		return 0
	case "config":
		return runConfig(runtime, options)
	case "status":
		return runStatus(ctx, runtime, options)
	case "doctor":
		return runDoctor(ctx, runtime, options)
	case "ls":
		return runList(ctx, runtime, options)
	case "new":
		return runNew(ctx, runtime, command, options)
	case "read":
		return runRead(ctx, runtime, command, options)
	case "send":
		return runSend(ctx, runtime, command, options)
	case "tail":
		return runLog(ctx, runtime, command, options, true)
	case "head":
		return runLog(ctx, runtime, command, options, false)
	case "kill":
		return runKill(ctx, runtime, command, options)
	case "rename":
		return runRename(ctx, runtime, command, options)
	default:
		fmt.Fprintf(options.Stderr, "interactive-terminal-mcp: %q has no one-shot handler\n", command.Name)
		return 2
	}
}
