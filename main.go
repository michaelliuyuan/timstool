package main

import (
	"github.com/michaelliuyuan/timstool/cmd"

	// Register source adapters (init() → source.Register).
	_ "github.com/michaelliuyuan/timstool/internal/source/mysql"
	_ "github.com/michaelliuyuan/timstool/internal/source/postgres"
)

func main() {
	cmd.Execute()
}
