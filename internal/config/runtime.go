package config

import "runtime"

// runtimeGOMAXPROCS returns the current GOMAXPROCS setting.
func runtimeGOMAXPROCS() int { return runtime.GOMAXPROCS(0) }
