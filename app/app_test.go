//go:build windows

package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"focusflash/agent"
	"focusflash/app"
)

type modelConfig struct {
	Name string `yaml:"name"`
}

type testModel struct {
	name string
}

func (m testModel) Generate(_ context.Context, _ agent.ModelRequest, output agent.ModelOutput) error {
	output.EmitText(m.name)
	return nil
}

func TestNewInjectsProjectModelConfiguration(t *testing.T) {
	projectDir := t.TempDir()
	path := filepath.Join(projectDir, "config.yaml")
	newModel := func(config modelConfig) (agent.Model, error) {
		return testModel{name: config.Name}, nil
	}

	for _, name := range []string{"first-model", "edited-model"} {
		if err := os.WriteFile(path, []byte("model:\n  name: "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		runtime, err := app.New(projectDir, newModel)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Run(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Messages) != 2 || result.Messages[1].Text != name {
			t.Fatalf("Run with model %q returned messages %+v", name, result.Messages)
		}
	}
}

func TestNewRejectsInvalidProjectConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name string
		yaml string
		want string
	}{
		{"empty", "", "model section is required"},
		{"null model", "model: null\n", "model section is required"},
		{"invalid YAML", "model: [\n", "yaml:"},
		{"unknown section", "model: {}\nagent: {}\n", "field agent not found"},
		{"unknown model field", "model:\n  nam: typo\n", "field nam not found"},
		{"wrong model type", "model: invalid\n", "cannot unmarshal"},
		{"unknown sandbox mode", "model: {}\nsandbox_mode: invalid\n", "invalid sandbox_mode"},
		{"empty sandbox mode", "model: {}\nsandbox_mode: ''\n", "invalid sandbox_mode"},
		{"null sandbox mode", "model: {}\nsandbox_mode: null\n", "sandbox_mode must be a string"},
		{"extra document", "model: {}\n---\nmodel: {}\n", "expected a single YAML document"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"), []byte(tt.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			runtime, err := app.New(projectDir, func(modelConfig) (agent.Model, error) {
				t.Fatal("constructed a model from invalid configuration")
				return nil, nil
			})
			if runtime != nil || err == nil || !strings.Contains(err.Error(), "config.yaml") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New returned runtime %v, error %v, want a config.yaml error containing %q", runtime, err, tt.want)
			}
		})
	}
}

func TestNewPropagatesSetupErrors(t *testing.T) {
	projectDir := t.TempDir()
	modelErr := errors.New("model name is required")
	newModel := func(modelConfig) (agent.Model, error) {
		return nil, modelErr
	}
	if runtime, err := app.New(projectDir, newModel); runtime != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New without config.yaml returned runtime %v, error %v", runtime, err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.yaml"), []byte("model: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if runtime, err := app.New(projectDir, newModel); runtime != nil || !errors.Is(err, modelErr) {
		t.Fatalf("New with a failed model constructor returned runtime %v, error %v", runtime, err)
	}
}
