package main

import (
	"fmt"
	"os"
	"strings"
)

// eachTarget runs act on every named project and reports how the batch went.
// One bad name does not stop the rest: closing four sessions and finding the
// third gone should still close the fourth, and the caller sees which ones
// failed rather than a batch that stopped somewhere in the middle.
func eachTarget(names []string, act func(name string) error) error {
	var failed []string
	for _, name := range names {
		if err := act(name); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d failed: %s", len(failed), len(names), strings.Join(failed, ", "))
}
