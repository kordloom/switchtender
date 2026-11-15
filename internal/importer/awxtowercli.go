package importer

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// towerCLIAsset is one entry of the list tower-cli receive writes, read only for the tag that marks
// it as that tool's.
type towerCLIAsset struct {
	// AssetType names the kind of object the entry is, such as job_template or workflow.
	AssetType any `json:"asset_type" yaml:"asset_type"`
}

// refuseTowerCLI returns an error naming the cause when data is the file tower-cli receive writes,
// and nil for anything else.
//
// tower-cli receive writes a list of assets, each tagged with asset_type, as JSON by default or as
// YAML when asked. awx export, which replaced it in AWX 13.0.0, writes one object keyed by
// resource, and that is the document this importer reads. A tower-cli file failed here as a parse
// error about an array where an object belonged, which tells an operator holding a real export
// that the file is broken, when it is whole and only from the older tool. It runs only once the
// document has failed to decode, so a file that reads costs nothing.
func refuseTowerCLI(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	var assets []towerCLIAsset
	switch {
	case bytes.HasPrefix(trimmed, []byte("[")):
		// A list entry that is not an object fails its own decode and leaves the rest in place, so
		// the error does not decide whether any entry carried the tag.
		_ = json.Unmarshal(trimmed, &assets)
	case bytes.HasPrefix(trimmed, []byte("-")):
		if yaml.Unmarshal(trimmed, &assets) != nil {
			return nil
		}
	default:
		return nil
	}
	for _, a := range assets {
		if a.AssetType != nil {
			return fmt.Errorf("%w: this file was written by tower-cli receive, which lists assets "+
				"each tagged with asset_type, and the AWX importer reads the document awx export "+
				"writes, an object keyed by resource. Run awx export > export.json, the command "+
				"that replaced tower-cli receive, and import that file", ErrNothingRecognized)
		}
	}
	return nil
}
