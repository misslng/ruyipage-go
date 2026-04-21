package adapter

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/misslng/ruyipage-go/internal/support"
)

const (
	chromeForTestingEndpoint = "https://googlechromelabs.github.io/chrome-for-testing"
	chromeForTestingStorage  = "https://storage.googleapis.com/chrome-for-testing-public"
	chromedriverDownloadTTL  = 30 * time.Second
)

var exactVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// EnsureChromedriver 按 version 解析/下载 chromedriver.exe 并返回缓存路径。
//
// version 支持：
//   - "" / "stable" / "latest"：使用 Chrome for Testing 的 LATEST_RELEASE_STABLE
//   - 大版本号，如 "131"：自动解析为精确版本
//   - 精确版本号，如 "131.0.6778.85"：直接使用
//
// 下载产物缓存在 %LOCALAPPDATA%\ruyipage\chromedriver\{version}\chromedriver.exe。
// 非精确版本的解析结果会缓存到 %LOCALAPPDATA%\ruyipage\chromedriver\_resolved\{key}.txt，
// 命中缓存且本地驱动仍在时可直接复用，避免重复请求 Chrome for Testing。
func EnsureChromedriver(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	isExact := exactVersionPattern.MatchString(trimmed)

	if !isExact {
		if cached, ok := lookupCachedResolvedVersion(version); ok {
			if driverPath, ok := existingChromedriver(cached); ok {
				return driverPath, nil
			}
		}
	}

	exactVersion, err := resolveChromedriverVersion(version)
	if err != nil {
		return "", err
	}

	cacheDir, err := chromedriverCacheDir(exactVersion)
	if err != nil {
		return "", err
	}
	driverPath := filepath.Join(cacheDir, "chromedriver.exe")
	if stat, err := os.Stat(driverPath); err == nil && stat.Size() > 0 {
		if !isExact {
			_ = storeCachedResolvedVersion(version, exactVersion)
		}
		return driverPath, nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", support.NewBrowserLaunchError("创建 chromedriver 缓存目录失败", err)
	}

	zipURL := fmt.Sprintf(
		"%s/%s/win64/chromedriver-win64.zip",
		chromeForTestingStorage, exactVersion,
	)
	zipPath := filepath.Join(cacheDir, "chromedriver-win64.zip")
	if err := downloadFile(zipURL, zipPath, 5*time.Minute); err != nil {
		_ = os.Remove(zipPath)
		return "", support.NewBrowserLaunchError(
			fmt.Sprintf("下载 chromedriver %s 失败 (%s)", exactVersion, zipURL), err,
		)
	}
	defer os.Remove(zipPath)

	if err := extractChromedriverExe(zipPath, driverPath); err != nil {
		_ = os.Remove(driverPath)
		return "", support.NewBrowserLaunchError("解压 chromedriver.exe 失败", err)
	}
	if !isExact {
		_ = storeCachedResolvedVersion(version, exactVersion)
	}
	return driverPath, nil
}

func existingChromedriver(exactVersion string) (string, bool) {
	cacheDir, err := chromedriverCacheDir(exactVersion)
	if err != nil {
		return "", false
	}
	driverPath := filepath.Join(cacheDir, "chromedriver.exe")
	stat, err := os.Stat(driverPath)
	if err != nil || stat.Size() == 0 {
		return "", false
	}
	return driverPath, true
}

func resolvedVersionCacheKey(version string) string {
	key := strings.ToLower(strings.TrimSpace(version))
	if key == "" || key == "latest" {
		key = "stable"
	}
	return key
}

func resolvedVersionCachePath(version string) (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if strings.TrimSpace(base) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", support.NewBrowserLaunchError("获取用户目录失败", err)
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "ruyipage", "chromedriver", "_resolved", resolvedVersionCacheKey(version)+".txt"), nil
}

func lookupCachedResolvedVersion(version string) (string, bool) {
	path, err := resolvedVersionCachePath(version)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	resolved := strings.TrimSpace(string(data))
	if !exactVersionPattern.MatchString(resolved) {
		return "", false
	}
	return resolved, true
}

func storeCachedResolvedVersion(version, resolved string) error {
	path, err := resolvedVersionCachePath(version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(resolved), 0o644)
}

func resolveChromedriverVersion(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if exactVersionPattern.MatchString(trimmed) {
		return trimmed, nil
	}

	endpoint := chromeForTestingEndpoint + "/LATEST_RELEASE_STABLE"
	switch strings.ToLower(trimmed) {
	case "", "stable", "latest":
		endpoint = chromeForTestingEndpoint + "/LATEST_RELEASE_STABLE"
	default:
		if !isPositiveMajorVersion(trimmed) {
			return "", support.NewBrowserLaunchError(
				fmt.Sprintf("无法识别的 chromedriver 版本 %q，请传 \"stable\"、大版本号（如 \"131\"）或精确版本（如 \"131.0.6778.85\"）", trimmed),
				nil,
			)
		}
		endpoint = chromeForTestingEndpoint + "/LATEST_RELEASE_" + trimmed
	}

	body, err := httpGetBytes(endpoint, chromedriverDownloadTTL)
	if err != nil {
		return "", support.NewBrowserLaunchError(
			fmt.Sprintf("解析 chromedriver 版本失败 (%s)", endpoint), err,
		)
	}
	resolved := strings.TrimSpace(string(body))
	if !exactVersionPattern.MatchString(resolved) {
		return "", support.NewBrowserLaunchError(
			fmt.Sprintf("Chrome for Testing 返回了非预期的版本字符串 %q", resolved), nil,
		)
	}
	return resolved, nil
}

func chromedriverCacheDir(version string) (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if strings.TrimSpace(base) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", support.NewBrowserLaunchError("获取用户目录失败", err)
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "ruyipage", "chromedriver", version), nil
}

func isPositiveMajorVersion(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func httpGetBytes(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func downloadFile(url string, dest string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return nil
}

func extractChromedriverExe(zipPath string, destPath string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.File {
		if filepath.Base(file.Name) != "chromedriver.exe" {
			continue
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		defer source.Close()

		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, source); err != nil {
			return err
		}
		return nil
	}
	return errors.New("zip 包内未找到 chromedriver.exe")
}
