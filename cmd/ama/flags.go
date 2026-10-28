package main

import (
	"errors"
	"flag"
	"os"
)

// errFlagHelp marks a "no error, just -h/-help was requested" exit: flag
// already printed usage, run should just return nil rather than also
// printing the error.
var errFlagHelp = errors.New("flag: help requested")

// newFlagSet builds the command's flag set, writing usage to w.
func newFlagSet(w *os.File) *flag.FlagSet {
	fset := flag.NewFlagSet("ama", flag.ContinueOnError)
	fset.SetOutput(w)
	return fset
}

// parseFlags defines and parses ama's only two flags: -config (falling back
// to AMA_CONFIG, then ./ama.yaml, all handled by config.Load/ResolvePath) and
// -version.
func parseFlags(fset *flag.FlagSet, args []string) (configPath string, showVersion bool, err error) {
	fset.StringVar(&configPath, "config", "", "path to ama.yaml (falls back to $AMA_CONFIG, then ./ama.yaml)")
	fset.BoolVar(&showVersion, "version", false, "print the ama version and exit")
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", false, errFlagHelp
		}
		return "", false, err
	}
	return configPath, showVersion, nil
}
