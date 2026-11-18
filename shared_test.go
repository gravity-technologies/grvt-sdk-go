package grvt

import (
	"errors"
	"os"
	"path/filepath"
)

// ReadShared reads the test data every SDK shares, by its path under the folder that holds contract/ and
// scenarios/ (ReadShared("scenarios/orderbook.json")). That folder is the monorepo's sdk/, found by walking up
// from the package directory; set GRVT_SDK_SHARED to use another one, e.g. from a copy of this SDK outside the
// monorepo. Exported so the grvt_test package can use it too; it exists only in tests.
func ReadShared(path string) ([]byte, error) {
	root, err := sharedRoot()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
}

func sharedRoot() (string, error) {
	if env := os.Getenv("GRVT_SDK_SHARED"); env != "" {
		return env, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isFile(filepath.Join(dir, "contract", "contract.json")) && isDir(filepath.Join(dir, "scenarios")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no contract/ and scenarios/ above this SDK; set GRVT_SDK_SHARED")
		}
		dir = parent
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
