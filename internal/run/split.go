package run

// RawPathSplit reports whether a run with these fields is a split whose inventory is named by a
// path instead of a stored inventory.
//
// A split lists the inventory's hosts before the run exists, so before any policy, hold, or
// approval reads it, and the listing opens the path as the user the server runs as. A stored
// inventory is content somebody with the right to save it already put there. A path is whatever
// its sender typed, so the entries that accept a run ask this before they hand one to the
// dispatcher. A split that names no inventory at all has nothing to open, and a stored inventory
// replaces the path.
func RawPathSplit(tool, path, inventoryID string, shards int) bool {
	return shards >= 2 && path != "" && inventoryID == "" && NormalizeTool(tool) == ToolAnsible
}
