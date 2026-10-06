// pigo-experience is a portable CLI and stdio plugin for evidence-backed memory.
package main

import (
	"github.com/smallnest/pigo/internal/experience"
	"os"
)

func main() { os.Exit(experience.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
