package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		return
	}
	t.Setenv("HOME", home)
}

func TestResolveManagedBinaryTargetFallsBackToCLIInstallLocation(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	binaryName := "koutaku"
	if runtime.GOOS == "windows" {
		binaryName = "koutaku.exe"
	}

	got, err := koutakuCLIManagedBinaryPath(binaryName)
	if err != nil {
		t.Fatalf("koutakuCLIManagedBinaryPath() error = %v", err)
	}

	want := filepath.Join(home, ".koutaku", "bin", binaryName)
	if got != want {
		t.Fatalf("koutakuCLIManagedBinaryPath() = %q, want %q", got, want)
	}
}

func TestIsWrapperManagedBinaryPathMatchesCLIInstallLocation(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	binaryName := "koutaku"
	if runtime.GOOS == "windows" {
		binaryName = "koutaku.exe"
	}

	path := filepath.Join(home, ".koutaku", "bin", binaryName)
	if !isWrapperManagedBinaryPath(path, binaryName) {
		t.Fatalf("expected %q to be recognized as wrapper-managed", path)
	}
}

func TestIsWrapperManagedBinaryPathMatchesCacheInstallLocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		localAppData := t.TempDir()
		t.Setenv("LOCALAPPDATA", localAppData)
		path := filepath.Join(localAppData, "koutaku", "v1.2.3", "bin", "koutaku.exe-0")
		if !isWrapperManagedBinaryPath(path, "koutaku.exe") {
			t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
		}
		return
	}

	home := t.TempDir()
	setTestHome(t, home)
	if runtime.GOOS == "linux" {
		xdg := filepath.Join(home, ".custom-cache")
		t.Setenv("XDG_CACHE_HOME", xdg)
		path := filepath.Join(xdg, "koutaku", "v1.2.3", "bin", "koutaku-0")
		if !isWrapperManagedBinaryPath(path, "koutaku") {
			t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
		}
		return
	}

	path := filepath.Join(home, "Library", "Caches", "koutaku", "v1.2.3", "bin", "koutaku-0")
	if !isWrapperManagedBinaryPath(path, "koutaku") {
		t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
	}
}

func TestStageUpdateBinaryCopiesIntoTargetDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetDir := filepath.Join(root, ".koutaku", "bin")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}

	downloadPath := filepath.Join(t.TempDir(), "downloaded-koutaku")
	if err := os.WriteFile(downloadPath, []byte("new-binary"), 0o600); err != nil {
		t.Fatalf("write download: %v", err)
	}

	targetPath := filepath.Join(targetDir, "koutaku")
	stagePath, err := stageUpdateBinary(downloadPath, targetPath, 0o755)
	if err != nil {
		t.Fatalf("stageUpdateBinary() error = %v", err)
	}

	if filepath.Dir(stagePath) != targetDir {
		t.Fatalf("stage path dir = %q, want %q", filepath.Dir(stagePath), targetDir)
	}
	if got, err := os.ReadFile(stagePath); err != nil {
		t.Fatalf("read staged file: %v", err)
	} else if string(got) != "new-binary" {
		t.Fatalf("staged file contents = %q, want %q", string(got), "new-binary")
	}
}
