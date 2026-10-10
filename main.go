// Command gotinker is an interactive Go playground for the terminal.
//
// Every input becomes part of a real Go program that is type-checked,
// compiled with the installed Go toolchain and run, so all of Go works:
// generics, iterators, the whole standard library and third-party modules.
package main

import (
	"bufio"
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/term"
)

//go:embed goprt/*.go
var goprtFS embed.FS

const version = "0.2.2"

func main() {
	expr := flag.String("e", "", "evaluate `code` and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `gotinker — an interactive Go playground

Usage:
  gotinker              start the playground
  gotinker file.go      load a Go file into the session, then start
  gotinker -e 'code'    evaluate code and exit
  ... | gotinker        evaluate Go code from stdin

Inside the playground, type :help for commands.

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("gotinker", version)
		return
	}

	u := newUI()
	s, err := newSession(u)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotinker:", err)
		os.Exit(1)
	}
	defer s.Close()

	interactive := *expr == "" && term.IsTerminal(int(os.Stdin.Fd()))
	if !interactive {
		s.warm()
	} else {
		go s.warm()
	}

	for _, file := range flag.Args() {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gotinker:", err)
			os.Exit(1)
		}
		evalInterruptible(s, string(data))
	}

	switch {
	case *expr != "":
		evalInterruptible(s, *expr)
	case !interactive:
		runScript(s, os.Stdin)
	default:
		newREPL(s).run()
	}
}

// evalInterruptible evaluates src, cancelling it on Ctrl-C.
func evalInterruptible(s *Session, src string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	if strings.HasPrefix(strings.TrimSpace(src), ":") {
		runCommand(s, nil, src)
		return
	}
	s.Eval(ctx, src)
}

// runScript evaluates piped input, grouping lines into complete inputs.
func runScript(s *Session, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 4<<20)
	var buf strings.Builder
	for sc.Scan() {
		buf.WriteString(sc.Text() + "\n")
		if inputComplete(buf.String()) {
			evalInterruptible(s, buf.String())
			buf.Reset()
		}
	}
	if strings.TrimSpace(buf.String()) != "" {
		evalInterruptible(s, buf.String())
	}
}
