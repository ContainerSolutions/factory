package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
)

// skipPluginInstall turns opencode's install of pluginPackage into cfg into a no-op (flags.go):
// an empty node_modules/ and a package-lock.json whose root depends on it, both new.
func skipPluginInstall(cfg string) error {
	if err := os.Mkdir(filepath.Join(cfg, nodeModulesDir), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{"lockfileVersion": 3, "packages": map[string]any{
		"": map[string]any{"dependencies": map[string]string{pluginPackage: pluginVersion}}}})
	if err != nil {
		return err
	}
	return clirun.WriteNew(filepath.Join(cfg, npmLockFile), append(b, '\n'))
}
