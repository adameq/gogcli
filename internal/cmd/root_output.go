package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/openclaw/gogcli/internal/errfmt"
	"github.com/openclaw/gogcli/internal/outfmt"
)

type errorOutputWriter struct {
	io.Writer
	wrote bool
}

func (w *errorOutputWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.wrote = w.wrote || n > 0
	return n, err
}

// Early failures may precede parsing the output flag. Skip values and literal
// arguments so a payload containing "--json" cannot select an output mode.
func earlyJSONMode(args []string, model *kong.Node, out io.Writer) bool {
	mode := outfmt.FromEnv()
	jsonSet, plainSet := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		enabled := true
		if hasValue {
			parsed, err := strconv.ParseBool(value)
			enabled = err == nil && parsed
		}
		switch name {
		case "--json", "--machine", "-j":
			mode.JSON, jsonSet = enabled, true
		case "--plain", "--tsv", "-p":
			mode.Plain, plainSet = enabled, true
		default:
			if !hasValue && earlyFlagTakesValue(name, model) {
				i++
			} else if !strings.HasPrefix(arg, "-") && model != nil {
				for _, child := range model.Children {
					if child.Name == arg || slices.Contains(child.Aliases, arg) {
						model = child
						break
					}
				}
			}
		}
	}
	if jsonSet && mode.JSON && !plainSet {
		mode.Plain = false
	}
	if plainSet && mode.Plain && !jsonSet {
		mode.JSON = false
	}
	if !jsonSet && !plainSet && !mode.JSON && !mode.Plain && envBool("GOG_AUTO_JSON") && !isTerminalWriter(out) {
		mode.JSON = true
	}
	return mode.JSON
}

func earlyFlagTakesValue(name string, model *kong.Node) bool {
	if model == nil {
		return globalFlagTakesValue(name)
	}
	for _, group := range model.AllFlags(false) {
		for _, flag := range group {
			if name == "--"+flag.Name || (flag.Short != 0 && name == "-"+string(flag.Short)) || slices.Contains(flag.Aliases, strings.TrimPrefix(name, "--")) {
				return !flag.IsBool()
			}
		}
	}
	return false
}

func emitJSONErrorEnvelope(w io.Writer, err error) {
	if err == nil || ExitCode(err) == 0 {
		return
	}
	code := ExitCode(err)
	class := "error"
	for name, candidate := range stableExitCodes() {
		if candidate == code {
			class = name
			break
		}
	}
	message := strings.TrimSpace(errfmt.Format(err))
	if message == "" {
		message = err.Error()
	}
	// Error metadata is never subject to --results-only or --select.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message, "class": class},
	})
}

func writeNoArgsHeader(w io.Writer) {
	exe, err := os.Executable()
	if err != nil {
		exe = "gog"
	}
	_, _ = fmt.Fprintf(w, "bin: %s\ndescription: Google Workspace from the terminal\n", exe)
}
