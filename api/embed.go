package api

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"go.yaml.in/yaml/v3"
)

//go:embed openapi.yaml
var source []byte

var Spec = mustJSON()

func mustJSON() []byte {
	var document map[string]any
	if err := yaml.Unmarshal(source, &document); err != nil {
		panic(fmt.Errorf("decode embedded OpenAPI YAML: %w", err))
	}
	data, err := json.Marshal(document)
	if err != nil {
		panic(fmt.Errorf("encode embedded OpenAPI JSON: %w", err))
	}
	return data
}
