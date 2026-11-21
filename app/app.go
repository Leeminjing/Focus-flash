// Package app assembles the Focus-flash runtime from project configuration.
package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"focusflash/agent"

	"go.yaml.in/yaml/v3"
)

// New reads config.yaml from the project root. The model constructor defines
// the schema of the model section and builds the model to inject into the Agent.
func New[ModelConfig any](projectDir string, newModel func(ModelConfig) (agent.Model, error)) (*agent.Agent, error) {
	if newModel == nil {
		return nil, errors.New("app requires a model constructor")
	}
	path := filepath.Join(projectDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config struct {
		Model *ModelConfig `yaml:"model"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil && err != io.EOF {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if config.Model == nil {
		return nil, fmt.Errorf("%s: model section is required", path)
	}
	if err := decoder.Decode(&yaml.Node{}); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return nil, fmt.Errorf("%s: expected a single YAML document", path)
	}

	model, err := newModel(*config.Model)
	if err != nil {
		return nil, fmt.Errorf("configure model from %s: %w", path, err)
	}
	return agent.New(model, agent.Config{})
}
