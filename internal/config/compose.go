package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ComposeFile struct {
	Services map[string]Service `yaml:"services"`
	Networks map[string]Network `yaml:"networks"`
	Volumes  map[string]Volume  `yaml:"volumes"`
}
type Network struct {
	Driver   string            `yaml:"driver"`
	External bool              `yaml:"external"`
	Labels   map[string]string `yaml:"labels"`
}
type Volume struct {
	Driver   string `yaml:"driver"`
	External bool   `yaml:"external"`
	Name     string `yaml:"name"`
}

type Service struct {
	Image       string            `yaml:"image"`
	Ports       []string          `yaml:"ports"`
	Volumes     []string          `yaml:"volumes"`
	Networks    []string          `yaml:"networks"`
	Environment map[string]string `yaml:"environment"`
	DependsOn   []string          `yaml:"depends_on"`
}

func Load(explicitPath string) (ComposeFile, error) {
	path, err := resolveComposeFile(explicitPath)
	if err != nil {
		return ComposeFile{}, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return ComposeFile{}, err
	}

	expanded := os.ExpandEnv(string(raw))

	var cfg ComposeFile
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return ComposeFile{}, fmt.Errorf("error leyendo %s: %w", path, err)
	}

	if len(cfg.Services) == 0 {
		return ComposeFile{}, fmt.Errorf("el archivo %s no tiene servicios definidos", path)
	}

	return cfg, nil
}

func resolveComposeFile(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("no se encontró el archivo %q: %w", explicitPath, err)
		}
		return explicitPath, nil
	}

	candidates := []string{
		"compose.yaml",
		"compose.yml",
		"docker-compose.yaml",
		"docker-compose.yml",
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no se encontró compose.yml ni docker-compose.yml en el directorio actual")
}
