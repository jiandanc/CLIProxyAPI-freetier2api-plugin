// packstore 打包 CPA 插件商店的归档文件与校验和。
//
// 用 Go 实现而不是 shell：`zip` / `sha256sum` 在 Windows runner 上不可用，
// 而这里必须三平台行为一致。
//
// 宿主 internal/pluginstore 的安装契约：
//
//   - 归档资产名：{id}_{version}_{goos}_{goarch}.zip
//     （version 是 tag 去掉前导 v，例如 tag v0.1.1 → 0.1.1）
//   - 校验和资产名必须**恰好**是 checksums.txt，格式为 `<sha256>  <文件名>`
//   - zip 内**根目录**必须有且仅有一个动态库，文件名必须是 {id}{ext}
//     或 {id}-v{version}{ext}；zip 内出现其它动态库会被拒绝
//   - 宿主会校验 zip 的 sha256，然后解包到 {pluginsDir}/{goos}/{goarch}/
//
// 两种模式：
//
//	打包：  packstore -lib dist/freetier2api-linux-amd64.so -id freetier2api \
//	                 -version 0.1.0 -goos linux -goarch amd64 -out-dir dist
//	校验和：packstore -checksums-dir dist      # 汇总目录下所有 *.zip → checksums.txt
//
// 汇总步骤必须单独做：矩阵里每个平台各生成一份 checksums.txt 会互相覆盖，
// 导致部分 zip 的校验和丢失、安装时直接失败。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if errRun := run(os.Args[1:], os.Stdout); errRun != nil {
		fmt.Fprintf(os.Stderr, "packstore: %v\n", errRun)
		os.Exit(1)
	}
}

// run 是入口实现：把参数与输出流作为参数传入，便于在不启子进程的情况下测试。
func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("packstore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	libPath := flags.String("lib", "", "动态库路径")
	pluginID := flags.String("id", "", "插件 id（必须与动态库文件名一致）")
	version := flags.String("version", "", "版本号（tag 去掉前导 v）")
	goos := flags.String("goos", "", "目标平台")
	goarch := flags.String("goarch", "", "目标架构")
	outDir := flags.String("out-dir", "", "输出目录")
	checksumsDir := flags.String("checksums-dir", "", "汇总模式：为目录下所有 *.zip 生成 checksums.txt")

	if errParse := flags.Parse(args); errParse != nil {
		return errParse
	}

	if *checksumsDir != "" {
		return writeChecksums(*checksumsDir, stdout)
	}
	if *libPath == "" || *pluginID == "" || *version == "" || *goos == "" || *goarch == "" {
		return fmt.Errorf("打包模式需要 -lib -id -version -goos -goarch")
	}
	if *outDir == "" {
		*outDir = "."
	}
	return packArchive(*libPath, *pluginID, *version, *goos, *goarch, *outDir, stdout)
}

// packArchive 生成 {id}_{version}_{goos}_{goarch}.zip。
func packArchive(libPath, pluginID, version, goos, goarch, outDir string, stdout io.Writer) error {
	if _, errStat := os.Stat(libPath); errStat != nil {
		return fmt.Errorf("动态库不存在: %w", errStat)
	}
	if errMkdir := os.MkdirAll(outDir, 0o755); errMkdir != nil {
		return fmt.Errorf("创建输出目录: %w", errMkdir)
	}

	archiveName := fmt.Sprintf("%s_%s_%s_%s.zip", pluginID, version, goos, goarch)
	archivePath := filepath.Join(outDir, archiveName)

	// zip 根目录内只放一个动态库，文件名用 {id}{ext}。
	entryName := pluginID + filepath.Ext(libPath)

	file, errCreate := os.Create(archivePath)
	if errCreate != nil {
		return fmt.Errorf("创建归档: %w", errCreate)
	}
	writer := zip.NewWriter(file)

	entry, errEntry := writer.Create(entryName)
	if errEntry != nil {
		_ = file.Close()
		return fmt.Errorf("创建归档条目: %w", errEntry)
	}
	source, errOpen := os.Open(libPath)
	if errOpen != nil {
		_ = file.Close()
		return fmt.Errorf("打开动态库: %w", errOpen)
	}
	if _, errCopy := io.Copy(entry, source); errCopy != nil {
		_ = source.Close()
		_ = file.Close()
		return fmt.Errorf("写入归档: %w", errCopy)
	}
	if errClose := source.Close(); errClose != nil {
		return fmt.Errorf("关闭动态库: %w", errClose)
	}
	if errClose := writer.Close(); errClose != nil {
		_ = file.Close()
		return fmt.Errorf("关闭归档: %w", errClose)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("关闭归档文件: %w", errClose)
	}

	fmt.Fprintf(stdout, "packed %s (entry=%s)\n", archiveName, entryName)
	return nil
}

// writeChecksums 汇总目录下所有 *.zip 的 sha256 到 checksums.txt。
func writeChecksums(dir string, stdout io.Writer) error {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return fmt.Errorf("读取目录: %w", errRead)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return fmt.Errorf("目录 %s 下没有 zip 归档", dir)
	}
	// 排序保证输出稳定（便于跨运行比对）。
	sort.Strings(names)

	var builder strings.Builder
	for _, name := range names {
		digest, errDigest := fileSHA256(filepath.Join(dir, name))
		if errDigest != nil {
			return errDigest
		}
		// 格式必须是 `<sha256>  <文件名>`（两个空格），宿主按此解析。
		fmt.Fprintf(&builder, "%s  %s\n", digest, name)
	}

	path := filepath.Join(dir, "checksums.txt")
	if errWrite := os.WriteFile(path, []byte(builder.String()), 0o644); errWrite != nil {
		return fmt.Errorf("写入 checksums.txt: %w", errWrite)
	}
	fmt.Fprint(stdout, builder.String())
	return nil
}

// fileSHA256 计算文件的 sha256。
func fileSHA256(path string) (string, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return "", fmt.Errorf("打开 %s: %w", path, errOpen)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	if _, errCopy := io.Copy(hasher, file); errCopy != nil {
		return "", fmt.Errorf("读取 %s: %w", path, errCopy)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
