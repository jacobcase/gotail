// Command gotail tails a file and writes lines to stdout, similar to `tail -f`.
// It uses the gotail watch and tail packages.
//
// Usage:
//
//	gotail [-start] [-stop] <path>
//
// Flags:
//
//	-start    start from the beginning of the file instead of the end
//	-stop     exit after reaching current EOF rather than following new data
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jacobcase/gotail/v3/tail"
)

func main() {
	start := flag.Bool("start", false, "start from beginning of file")
	stop := flag.Bool("stop", false, "exit after reaching EOF")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gotail [-start] [-stop] <path>\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}
	path := flag.Arg(0)

	whence := io.SeekEnd
	if *start {
		whence = io.SeekStart
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Whence:    whence,
		StopAtEOF: *stop,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tr, err := tail.New(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gotail: %v\n", err)
		os.Exit(1)
	}
	defer tr.Close()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for rec, err := range tr.Records(ctx) {
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(os.Stderr, "gotail: %v\n", err)
			os.Exit(1)
		}
		out.Write(rec.Line)
		out.WriteByte('\n')
	}
}
