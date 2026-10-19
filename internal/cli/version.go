package cli

import "runtime/debug"

// ResolveVersion returns the stamped version, or for a `go install` build
// (which applies no linker flags) the module version Go recorded, such as
// v0.1.0. A local build without either reports "dev".
func ResolveVersion(stamped string) string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(stamped, info)
}

func resolveVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
