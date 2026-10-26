// Command checkelf verifies that ELF binaries are fully static: no program
// interpreter and no shared-library dependencies.
package main

import (
	"debug/elf"
	"fmt"
	"os"
)

func main() {
	bad := false
	for _, path := range os.Args[1:] {
		f, err := elf.Open(path)
		if err != nil {
			fmt.Printf("%s: %v\n", path, err)
			bad = true
			continue
		}
		interp := false
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				interp = true
			}
		}
		libs, _ := f.ImportedLibraries()
		status := "static"
		if interp || len(libs) > 0 {
			status = fmt.Sprintf("DYNAMIC (interpreter=%v, libs=%v)", interp, libs)
			bad = true
		}
		fmt.Printf("%-28s %-8s %-10s %s\n", path, f.Class, f.Machine, status)
		f.Close()
	}
	if bad {
		os.Exit(1)
	}
}
