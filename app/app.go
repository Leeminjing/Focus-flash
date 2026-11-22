//go:build windows

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
	"focusflash/tool"

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
		Model       *ModelConfig `yaml:"model"`
		SandboxMode yaml.Node    `yaml:"sandbox_mode"`
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

	var mode *string
	if config.SandboxMode.Kind != 0 {
		if config.SandboxMode.Kind != yaml.ScalarNode || config.SandboxMode.ShortTag() != "!!str" {
			return nil, fmt.Errorf("configure tools from %s: sandbox_mode must be a string", path)
		}
		mode = &config.SandboxMode.Value
	}
	tools, err := tool.NewBuiltinOrchestrator(projectDir, mode)
	if err != nil {
		return nil, fmt.Errorf("configure tools from %s: %w", path, err)
	}
	model, err := newModel(*config.Model)
	if err != nil {
		return nil, fmt.Errorf("configure model from %s: %w", path, err)
	}
	return agent.New(model, agent.Config{Tools: tools})
}
