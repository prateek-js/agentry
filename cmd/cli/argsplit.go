package main

import "strings"

// splitFlagsAndPositionals divides an arg slice into flags and
// positionals. Flags start with `-` (including `--`); a flag without
// `=` consumes the next non-flag token as its value. Lone `-` is a
// positional (commonly "read from stdin"). Used by subcommands that
// accept both positional args and a flag bag without committing to a
// real flag parser.
//
// Lived in cmd/cli/forward.go before — moved here when forward got
// removed so other subcommands (pull, service, …) keep building.
func splitFlagsAndPositionals(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, positionals
}
