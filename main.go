package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const version = "1.9.0"

const flattenSeparator = "__"

const (
	retainedByType          = "指定保留类型"
	retainedByPath          = "路径关键词"
	retainedByDirectory     = "路径关键词（目录树）"
	retainedByContent       = "内容关键词"
	retainedByContentSize   = "超过内容检查上限"
	retainedByContentError  = "内容检查失败"
	retainedUnsupportedType = "不支持的特殊类型"
	retainedAtRoot          = "已在根目录"
	retainedUnselectedType  = "未选择的类型"
	retainedFlattenLink     = "链接不参与展平"
	retainedFlattenTooLong  = "展平文件名过长"
)

type config struct {
	root              string
	extensions        string
	exclusions        string
	contentExclusions string
	contentMaxMiB     int64
	noExtension       bool
	invert            bool
	deleteFiles       bool
	flatten           bool
	apply             bool
	yes               bool
	workers           int
	preview           int
	keepPreview       int
	emptyDirs         bool
	emptyDirPreview   int
	logPath           string
	ignoreScanErrors  bool
	showHelp          bool
	showVersion       bool
}

type candidate struct {
	path       string
	size       int64
	pattern    string
	isLinkLike bool
	target     string
	targetBase string
}

type extensionStats struct {
	count int64
	size  int64
}

type operationError struct {
	path string
	err  error
}

type retainedItem struct {
	path   string
	reason string
}

type emptyDirectory struct {
	path            string
	afterProcessing bool
}

type directoryState struct {
	blocked bool
	changes bool
}

type directoryTracker struct {
	enabled bool
	root    string
	order   []string
	states  map[string]*directoryState
}

type scanResult struct {
	visited              int64
	files                []candidate
	byPattern            map[string]extensionStats
	zeroSize             int64
	linkLikes            int64
	excluded             int64
	excludedDirs         int64
	kept                 int64
	contentFilterEnabled bool
	contentChecked       int64
	contentBytes         int64
	contentExcluded      int64
	contentTooLarge      int64
	unsupported          int64
	retainedTotal        int64
	retained             []retainedItem
	emptyDirs            []emptyDirectory
	errors               []operationError
}

type flattenScanResult struct {
	scanResult
	rootFiles     int64
	linksRetained int64
	numbered      int64
	tooLong       int64
}

type deleteResult struct {
	file   candidate
	status string
	reason string
	err    error
}

type deleteSummary struct {
	deleted     int64
	deletedSize int64
	linkLikes   int64
	skipped     int64
	failed      int64
}

type moveResult struct {
	file   candidate
	status string
	reason string
	err    error
}

type moveSummary struct {
	moved     int64
	movedSize int64
	skipped   int64
	failed    int64
}

type directoryDeleteSummary struct {
	deleted int64
	skipped int64
	failed  int64
}

type progressDisplay struct {
	out         io.Writer
	interactive bool
	lastUpdate  time.Time
	reporter    progressReporter
	lastReport  time.Time
}

type progressState struct {
	phase     string
	processed int64
	total     int64
	selected  int64
	succeeded int64
	failed    int64
	done      bool
}

type progressReporter interface {
	reportProgress(progressState)
}

type contentFilter struct {
	keywords [][]byte
	maxBytes int64
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, ok := parseFlags(args, stderr)
	if !ok {
		return 2
	}
	if cfg.showHelp {
		return 0
	}
	if cfg.showVersion {
		fmt.Fprintf(stdout, "filetype-cleaner %s\n", version)
		return 0
	}

	extensionInput := cfg.extensions
	if cfg.noExtension {
		if strings.TrimSpace(extensionInput) == "" {
			extensionInput = "none"
		} else {
			extensionInput += ",none"
		}
	}
	var patterns []string
	var err error
	if strings.TrimSpace(extensionInput) != "" {
		patterns, err = normalizeExtensions(extensionInput)
		if err != nil {
			fmt.Fprintf(stderr, "参数错误: %v\n", err)
			return 2
		}
	}
	exclusions, err := normalizeExclusions(cfg.exclusions)
	if err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	contentExclusions, err := normalizeContentExclusions(cfg.contentExclusions)
	if err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return 2
	}
	filter := newContentFilter(contentExclusions, cfg.contentMaxMiB)

	root, err := validateRoot(cfg.root)
	if err != nil {
		fmt.Fprintf(stderr, "目录错误: %v\n", err)
		return 2
	}
	if cfg.flatten {
		return runFlatten(cfg, root, patterns, len(patterns) > 0, exclusions, filter, stdin, stdout, stderr)
	}
	if cfg.deleteFiles && isVolumeRoot(root) {
		fmt.Fprintln(stderr, "安全限制: 不允许直接在磁盘根目录执行删除，请指定一个子目录。")
		return 2
	}

	if cfg.invert {
		fmt.Fprintf(stdout, "反向模式：仅保留 %s，其他类型进入处理列表。\n", displayPatterns(patterns))
	}
	fmt.Fprintf(stdout, "正在扫描: %s\n", root)
	result := scan(root, patterns, exclusions, cfg.invert, filter, cfg.keepPreview, cfg.emptyDirs, stderr)
	printScanSummary(stdout, result, cfg.preview, cfg.emptyDirPreview, cfg.invert)
	printErrors(stderr, "扫描错误", result.errors, 10)

	if len(result.files) == 0 && len(result.emptyDirs) == 0 {
		fmt.Fprintln(stdout, "没有需要处理的文件或空文件夹，未执行删除。")
		if len(result.errors) > 0 {
			return 1
		}
		return 0
	}

	if !cfg.deleteFiles {
		fmt.Fprintln(stdout, "预览模式：没有删除任何文件。确认无误后增加 --delete 参数。")
		if len(result.errors) > 0 {
			return 1
		}
		return 0
	}

	if len(result.errors) > 0 && !cfg.ignoreScanErrors {
		fmt.Fprintln(stderr, "扫描不完整，已取消删除。检查权限/路径，或确认风险后使用 --ignore-scan-errors。")
		return 1
	}

	if !cfg.yes && !confirmDeletion(stdin, stdout, len(result.files), len(result.emptyDirs), root, cfg.invert) {
		fmt.Fprintln(stdout, "已取消，没有删除任何文件。")
		return 0
	}

	var logFile *os.File
	if cfg.logPath != "" {
		logFile, err = os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "无法打开日志文件: %v\n", err)
			return 1
		}
		defer logFile.Close()
		fmt.Fprintf(logFile, "# filetype-cleaner %s\t%s\troot=%s\tinvert=%t\tempty_dirs=%t\n", version, time.Now().Format(time.RFC3339), root, cfg.invert, cfg.emptyDirs)
	}

	var summary deleteSummary
	var deleteErrors []operationError
	if len(result.files) > 0 {
		summary, deleteErrors = deleteCandidates(result.files, patterns, cfg.invert, filter, cfg.workers, logFile, stderr)
	}
	var directorySummary directoryDeleteSummary
	var directoryErrors []operationError
	if len(result.emptyDirs) > 0 {
		directorySummary, directoryErrors = deleteEmptyDirectories(result.emptyDirs, logFile, stderr)
	}
	fmt.Fprintln(stdout, "删除完成")
	fmt.Fprintf(stdout, "  已删除: %s 个 (%s)\n", formatInteger(summary.deleted), formatBytes(summary.deletedSize))
	if summary.linkLikes > 0 {
		fmt.Fprintf(stdout, "  其中链接/重解析点: %s 个\n", formatInteger(summary.linkLikes))
	}
	fmt.Fprintf(stdout, "  已跳过: %s 个\n", formatInteger(summary.skipped))
	fmt.Fprintf(stdout, "  失败:   %s 个\n", formatInteger(summary.failed))
	if cfg.emptyDirs {
		fmt.Fprintf(stdout, "  空文件夹已删除: %s 个\n", formatInteger(directorySummary.deleted))
		fmt.Fprintf(stdout, "  空文件夹已跳过: %s 个\n", formatInteger(directorySummary.skipped))
		fmt.Fprintf(stdout, "  空文件夹失败:   %s 个\n", formatInteger(directorySummary.failed))
	}
	printErrors(stderr, "删除错误", deleteErrors, 20)
	printErrors(stderr, "空文件夹删除错误", directoryErrors, 20)
	if cfg.logPath != "" {
		fmt.Fprintf(stdout, "  日志:   %s\n", cfg.logPath)
	}
	if summary.failed > 0 || directorySummary.failed > 0 {
		return 1
	}
	return 0
}

func runFlatten(cfg config, root string, patterns []string, hasSelection bool, exclusions []string, filter contentFilter, stdin io.Reader, stdout, stderr io.Writer) int {
	if cfg.apply && isVolumeRoot(root) {
		fmt.Fprintln(stderr, "安全限制: 不允许直接在磁盘根目录执行展平，请指定一个子目录。")
		return 2
	}
	if cfg.invert {
		fmt.Fprintf(stdout, "反向模式：仅保留 %s，其他类型进入展平列表。\n", displayPatterns(patterns))
	} else if !hasSelection {
		fmt.Fprintln(stdout, "展平模式：所有子目录中的普通文件进入展平列表。")
	}
	fmt.Fprintf(stdout, "正在扫描展平计划: %s\n", root)
	result := scanFlatten(root, patterns, hasSelection, exclusions, cfg.invert, filter, cfg.keepPreview, cfg.emptyDirs, stderr)
	printFlattenSummary(stdout, result, cfg.preview, cfg.emptyDirPreview)
	printErrors(stderr, "扫描错误", result.errors, 10)

	if len(result.files) == 0 && len(result.emptyDirs) == 0 {
		fmt.Fprintln(stdout, "没有需要移动的文件或空文件夹。")
		if len(result.errors) > 0 {
			return 1
		}
		return 0
	}
	if !cfg.apply {
		fmt.Fprintln(stdout, "预览模式：没有移动文件或删除空文件夹。确认无误后增加 --apply 参数。")
		if len(result.errors) > 0 {
			return 1
		}
		return 0
	}
	if len(result.errors) > 0 && !cfg.ignoreScanErrors {
		fmt.Fprintln(stderr, "扫描不完整，已取消展平。检查权限/路径，或确认风险后使用 --ignore-scan-errors。")
		return 1
	}
	if !cfg.yes && !confirmFlatten(stdin, stdout, len(result.files), len(result.emptyDirs), root) {
		fmt.Fprintln(stdout, "已取消，没有移动文件或删除空文件夹。")
		return 0
	}

	var logFile *os.File
	var err error
	if cfg.logPath != "" {
		logFile, err = os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "无法打开日志文件: %v\n", err)
			return 1
		}
		defer logFile.Close()
		fmt.Fprintf(logFile, "# filetype-cleaner %s\t%s\troot=%s\tmode=flatten\tinvert=%t\tempty_dirs=%t\n", version, time.Now().Format(time.RFC3339), root, cfg.invert, cfg.emptyDirs)
	}

	var summary moveSummary
	var moveErrors []operationError
	if len(result.files) > 0 {
		summary, moveErrors = moveCandidates(result.files, root, patterns, hasSelection, cfg.invert, filter, cfg.workers, logFile, stderr)
	}
	var directorySummary directoryDeleteSummary
	var directoryErrors []operationError
	if len(result.emptyDirs) > 0 {
		directorySummary, directoryErrors = deleteEmptyDirectories(result.emptyDirs, logFile, stderr)
	}
	fmt.Fprintln(stdout, "展平完成")
	fmt.Fprintf(stdout, "  已移动: %s 个 (%s)\n", formatInteger(summary.moved), formatBytes(summary.movedSize))
	fmt.Fprintf(stdout, "  已跳过: %s 个\n", formatInteger(summary.skipped))
	fmt.Fprintf(stdout, "  失败:   %s 个\n", formatInteger(summary.failed))
	if cfg.emptyDirs {
		fmt.Fprintf(stdout, "  空文件夹已删除: %s 个\n", formatInteger(directorySummary.deleted))
		fmt.Fprintf(stdout, "  空文件夹已跳过: %s 个\n", formatInteger(directorySummary.skipped))
		fmt.Fprintf(stdout, "  空文件夹失败:   %s 个\n", formatInteger(directorySummary.failed))
	}
	printErrors(stderr, "移动错误", moveErrors, 20)
	printErrors(stderr, "空文件夹删除错误", directoryErrors, 20)
	if cfg.logPath != "" {
		fmt.Fprintf(stdout, "  日志:   %s\n", cfg.logPath)
	}
	if summary.failed > 0 || directorySummary.failed > 0 {
		return 1
	}
	return 0
}

func parseFlags(args []string, stderr io.Writer) (config, bool) {
	var cfg config
	defaultWorkers := runtime.NumCPU()
	if defaultWorkers > 4 {
		defaultWorkers = 4
	}
	if defaultWorkers < 1 {
		defaultWorkers = 1
	}

	flags := flag.NewFlagSet("filetype-cleaner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.root, "root", "", "要扫描的根目录（必填）")
	flags.StringVar(&cfg.extensions, "ext", "", "扩展名列表，例如 .c,.h 或 c,h；none 表示无扩展名")
	flags.StringVar(&cfg.extensions, "types", "", "--ext 的别名")
	flags.StringVar(&cfg.exclusions, "exclude", "", "排除相对路径中含有的关键词，多个用逗号分隔（忽略大小写）")
	flags.StringVar(&cfg.contentExclusions, "exclude-content", "", "排除内容中含有关键词的文件，多个用逗号分隔（忽略大小写）")
	flags.Int64Var(&cfg.contentMaxMiB, "content-max-mib", 16, "内容检查的单文件上限 MiB；0 表示不限制")
	flags.BoolVar(&cfg.noExtension, "no-ext", false, "同时匹配无扩展名文件；可独立使用")
	flags.BoolVar(&cfg.invert, "invert", false, "反向模式：保留指定类型，处理其他类型")
	flags.BoolVar(&cfg.deleteFiles, "delete", false, "实际删除；不指定时仅预览")
	flags.BoolVar(&cfg.flatten, "flatten", false, "展平模式：将子目录文件移到根目录，并把原路径加入文件名")
	flags.BoolVar(&cfg.apply, "apply", false, "实际执行展平移动；不指定时仅预览")
	flags.BoolVar(&cfg.yes, "yes", false, "跳过 DELETE/MOVE 确认，适用于自动化")
	flags.IntVar(&cfg.workers, "workers", defaultWorkers, "删除或移动并发数（1-64）")
	flags.IntVar(&cfg.preview, "preview", 20, "预览路径数量，0 表示不显示")
	flags.IntVar(&cfg.keepPreview, "keep-preview", 20, "保留路径预览数量，0 表示不显示")
	flags.BoolVar(&cfg.emptyDirs, "empty-dirs", false, "同时处理当前为空或处理后变空的文件夹")
	flags.IntVar(&cfg.emptyDirPreview, "empty-dir-preview", 20, "空文件夹预览数量，0 表示不显示")
	flags.StringVar(&cfg.logPath, "log", "", "将处理结果和错误追加到指定日志文件")
	flags.BoolVar(&cfg.ignoreScanErrors, "ignore-scan-errors", false, "扫描有错误时仍处理已找到的文件")
	flags.BoolVar(&cfg.showVersion, "version", false, "显示版本")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "按扩展名安全删除文件，或将子目录文件展平到根目录。默认只预览。")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "用法:")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --ext .c,.h")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --ext .c,.h --exclude vendor,temp")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --ext .c,.h --exclude-content generated")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --ext .c,.h --invert")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --empty-dirs")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --no-ext")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --ext .c,.h --delete")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --flatten")
		fmt.Fprintln(stderr, "  filetype-cleaner --root D:\\rom_workspace --flatten --apply")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "参数:")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config{showHelp: true}, true
		}
		return config{}, false
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "参数错误: 不支持位置参数，请使用 --root 和 --ext。")
		return config{}, false
	}
	if cfg.showVersion {
		return cfg, true
	}
	if strings.TrimSpace(cfg.root) == "" {
		fmt.Fprintln(stderr, "参数错误: 必须指定 --root。")
		flags.Usage()
		return config{}, false
	}
	hasFileSelection := strings.TrimSpace(cfg.extensions) != "" || cfg.noExtension
	if !hasFileSelection && !cfg.emptyDirs && !cfg.flatten {
		fmt.Fprintln(stderr, "参数错误: 必须指定 --ext、--no-ext、--empty-dirs 或 --flatten。")
		flags.Usage()
		return config{}, false
	}
	if cfg.flatten && cfg.deleteFiles {
		fmt.Fprintln(stderr, "参数错误: --flatten 不能与 --delete 同时使用；展平请使用 --apply。")
		return config{}, false
	}
	if cfg.apply && !cfg.flatten {
		fmt.Fprintln(stderr, "参数错误: --apply 必须与 --flatten 同时使用。")
		return config{}, false
	}
	if cfg.invert && !hasFileSelection {
		fmt.Fprintln(stderr, "参数错误: --invert 必须同时指定 --ext 或 --no-ext。")
		return config{}, false
	}
	if cfg.workers < 1 || cfg.workers > 64 {
		fmt.Fprintln(stderr, "参数错误: --workers 必须在 1 到 64 之间。")
		return config{}, false
	}
	if cfg.preview < 0 || cfg.preview > 1000 {
		fmt.Fprintln(stderr, "参数错误: --preview 必须在 0 到 1000 之间。")
		return config{}, false
	}
	if cfg.keepPreview < 0 || cfg.keepPreview > 1000 {
		fmt.Fprintln(stderr, "参数错误: --keep-preview 必须在 0 到 1000 之间。")
		return config{}, false
	}
	if cfg.emptyDirPreview < 0 || cfg.emptyDirPreview > 1000 {
		fmt.Fprintln(stderr, "参数错误: --empty-dir-preview 必须在 0 到 1000 之间。")
		return config{}, false
	}
	if cfg.contentMaxMiB < 0 || cfg.contentMaxMiB > 1024*1024 {
		fmt.Fprintln(stderr, "参数错误: --content-max-mib 必须在 0 到 1048576 之间。")
		return config{}, false
	}
	return cfg, true
}

func normalizeExtensions(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if item == "none" || item == "<none>" || item == "[none]" {
			seen[""] = struct{}{}
			continue
		}
		if strings.HasPrefix(item, "*.") {
			item = item[1:]
		}
		if strings.ContainsAny(item, "*/\\") {
			return nil, fmt.Errorf("无效扩展名 %q；只接受 .c、c 或 *.c 形式", item)
		}
		if !strings.HasPrefix(item, ".") {
			item = "." + item
		}
		if item == "." {
			return nil, fmt.Errorf("扩展名不能只有一个点")
		}
		seen[item] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("没有有效的扩展名")
	}

	patterns := make([]string, 0, len(seen))
	for item := range seen {
		patterns = append(patterns, item)
	}
	// Longest first makes .tar.gz win over .gz when both are provided.
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) == len(patterns[j]) {
			return patterns[i] < patterns[j]
		}
		return len(patterns[i]) > len(patterns[j])
	})
	return patterns, nil
}

func normalizeExclusions(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		item = strings.ReplaceAll(item, "/", string(filepath.Separator))
		item = strings.ReplaceAll(item, "\\", string(filepath.Separator))
		seen[strings.ToLower(item)] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("--exclude 没有有效关键词")
	}

	exclusions := make([]string, 0, len(seen))
	for item := range seen {
		exclusions = append(exclusions, item)
	}
	sort.Strings(exclusions)
	return exclusions, nil
}

func normalizeContentExclusions(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" {
			seen[item] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("--exclude-content 没有有效关键词")
	}

	keywords := make([]string, 0, len(seen))
	for item := range seen {
		keywords = append(keywords, item)
	}
	sort.Strings(keywords)
	return keywords, nil
}

func newContentFilter(keywords []string, maxMiB int64) contentFilter {
	filter := contentFilter{maxBytes: maxMiB * 1024 * 1024}
	for _, keyword := range keywords {
		filter.keywords = append(filter.keywords, []byte(keyword))
	}
	return filter
}

func (filter contentFilter) enabled() bool {
	return len(filter.keywords) > 0
}

func (filter contentFilter) match(path string, knownSize int64) (matched, tooLarge bool, bytesRead int64, err error) {
	if !filter.enabled() {
		return false, false, 0, nil
	}
	if filter.maxBytes > 0 && knownSize > filter.maxBytes {
		return false, true, 0, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return false, false, 0, err
	}
	defer file.Close()

	var reader io.Reader = file
	if filter.maxBytes > 0 {
		reader = io.LimitReader(file, filter.maxBytes+1)
	}

	maxKeywordBytes := 0
	for _, keyword := range filter.keywords {
		if len(keyword) > maxKeywordBytes {
			maxKeywordBytes = len(keyword)
		}
	}
	buffer := make([]byte, 64*1024)
	var tail []byte
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			bytesRead += int64(n)
			if filter.maxBytes > 0 && bytesRead > filter.maxBytes {
				return false, true, bytesRead, nil
			}
			window := make([]byte, len(tail)+n)
			copy(window, tail)
			copy(window[len(tail):], buffer[:n])
			lowerContent := bytes.ToLower(window)
			for _, keyword := range filter.keywords {
				if bytes.Contains(lowerContent, keyword) {
					return true, false, bytesRead, nil
				}
			}

			tailBytes := maxKeywordBytes - 1
			if tailBytes > len(window) {
				tailBytes = len(window)
			}
			tail = append(tail[:0], window[len(window)-tailBytes:]...)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, false, bytesRead, readErr
		}
	}
	return false, false, bytesRead, nil
}

func validateRoot(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是目录", abs)
	}
	return abs, nil
}

func isVolumeRoot(path string) bool {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return filepath.Clean(path) == string(filepath.Separator)
	}
	return filepath.Clean(path) == filepath.Clean(volume+string(filepath.Separator))
}

func matchPattern(name string, patterns []string) (string, bool) {
	lowerName := strings.ToLower(name)
	for _, pattern := range patterns {
		if pattern == "" {
			if filepath.Ext(name) == "" {
				return pattern, true
			}
			continue
		}
		if strings.HasSuffix(lowerName, pattern) {
			return pattern, true
		}
	}
	return "", false
}

func scan(root string, patterns, exclusions []string, invert bool, filter contentFilter, keepPreview int, trackEmptyDirs bool, progress io.Writer) scanResult {
	result := scanResult{
		byPattern:            make(map[string]extensionStats),
		contentFilterEnabled: filter.enabled(),
	}
	directories := newDirectoryTracker(root, trackEmptyDirs)
	display := newProgressDisplay(progress)
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		result.visited++
		display.scan(result.visited, int64(len(result.files)), invert, false)
		if walkErr != nil {
			result.errors = append(result.errors, operationError{path: path, err: walkErr})
			directories.blockForError(path)
			return nil
		}
		if isExcludedPath(root, path, exclusions) {
			result.excluded++
			directories.blockEntry(path)
			if entry.IsDir() {
				result.excludedDirs++
				result.addRetained(path, retainedByDirectory, keepPreview)
				return filepath.SkipDir
			}
			result.addRetained(path, retainedByPath, keepPreview)
			return nil
		}
		if entry.IsDir() {
			directories.add(path)
			return nil
		}
		pattern, matched := matchPattern(entry.Name(), patterns)
		if invert && matched {
			result.kept++
			result.addRetained(path, retainedByType, keepPreview)
			directories.blockEntry(path)
			return nil
		}
		if !invert && !matched {
			directories.blockEntry(path)
			return nil
		}
		if invert {
			pattern = actualExtension(entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			result.errors = append(result.errors, operationError{path: path, err: err})
			directories.blockEntry(path)
			return nil
		}
		isLinkLike, supported := classifyFileInfo(info)
		if !supported {
			result.unsupported++
			result.addRetained(path, retainedUnsupportedType, keepPreview)
			directories.blockEntry(path)
			return nil
		}
		if filter.enabled() && !isLinkLike {
			contentMatched, tooLarge, bytesRead, err := filter.match(path, info.Size())
			result.contentBytes += bytesRead
			if err != nil {
				result.errors = append(result.errors, operationError{path: path, err: fmt.Errorf("内容检查: %w", err)})
				result.addRetained(path, retainedByContentError, keepPreview)
				directories.blockEntry(path)
				return nil
			}
			if tooLarge {
				result.contentTooLarge++
				result.addRetained(path, retainedByContentSize, keepPreview)
				directories.blockEntry(path)
				return nil
			}
			result.contentChecked++
			if contentMatched {
				result.contentExcluded++
				result.addRetained(path, retainedByContent, keepPreview)
				directories.blockEntry(path)
				return nil
			}
		}
		file := candidate{path: path, size: info.Size(), pattern: pattern, isLinkLike: isLinkLike}
		result.files = append(result.files, file)
		if info.Size() == 0 {
			result.zeroSize++
		}
		if isLinkLike {
			result.linkLikes++
		}
		stats := result.byPattern[pattern]
		stats.count++
		stats.size += info.Size()
		result.byPattern[pattern] = stats
		directories.removeEntry(path)
		return nil
	})
	result.emptyDirs = directories.emptyDirectories()
	display.scan(result.visited, int64(len(result.files)), invert, true)
	return result
}

func scanFlatten(root string, patterns []string, hasSelection bool, exclusions []string, invert bool, filter contentFilter, keepPreview int, trackEmptyDirs bool, progress io.Writer) flattenScanResult {
	result := flattenScanResult{scanResult: scanResult{
		byPattern:            make(map[string]extensionStats),
		contentFilterEnabled: filter.enabled(),
	}}
	directories := newDirectoryTracker(root, trackEmptyDirs)
	display := newProgressDisplay(progress)
	occupied := make(map[string]struct{})
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		result.errors = append(result.errors, operationError{path: root, err: fmt.Errorf("读取根目录: %w", err)})
		directories.blockForError(root)
	} else {
		for _, entry := range rootEntries {
			occupied[flattenCollisionKey(entry.Name())] = struct{}{}
		}
	}

	var provisional []candidate
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		result.visited++
		display.scan(result.visited, int64(len(provisional)), invert, false)
		if walkErr != nil {
			result.errors = append(result.errors, operationError{path: path, err: walkErr})
			directories.blockForError(path)
			return nil
		}
		if isExcludedPath(root, path, exclusions) {
			result.excluded++
			directories.blockEntry(path)
			if entry.IsDir() {
				result.excludedDirs++
				result.addRetained(path, retainedByDirectory, keepPreview)
				return filepath.SkipDir
			}
			result.addRetained(path, retainedByPath, keepPreview)
			return nil
		}
		if entry.IsDir() {
			directories.add(path)
			return nil
		}
		if filepath.Dir(path) == root {
			result.rootFiles++
			result.addRetained(path, retainedAtRoot, keepPreview)
			return nil
		}

		pattern, matched := matchPattern(entry.Name(), patterns)
		selected := true
		if hasSelection {
			selected = matched
			if invert {
				selected = !matched
			}
		}
		if !selected {
			result.kept++
			reason := retainedUnselectedType
			if invert && matched {
				reason = retainedByType
			}
			result.addRetained(path, reason, keepPreview)
			directories.blockEntry(path)
			return nil
		}
		if !matched {
			pattern = actualExtension(entry.Name())
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			result.errors = append(result.errors, operationError{path: path, err: infoErr})
			directories.blockEntry(path)
			return nil
		}
		isLinkLike, supported := classifyFileInfo(info)
		if !supported {
			result.unsupported++
			result.addRetained(path, retainedUnsupportedType, keepPreview)
			directories.blockEntry(path)
			return nil
		}
		if isLinkLike {
			result.linksRetained++
			result.addRetained(path, retainedFlattenLink, keepPreview)
			directories.blockEntry(path)
			return nil
		}
		if filter.enabled() {
			contentMatched, tooLarge, bytesRead, matchErr := filter.match(path, info.Size())
			result.contentBytes += bytesRead
			if matchErr != nil {
				result.errors = append(result.errors, operationError{path: path, err: fmt.Errorf("内容检查: %w", matchErr)})
				result.addRetained(path, retainedByContentError, keepPreview)
				directories.blockEntry(path)
				return nil
			}
			if tooLarge {
				result.contentTooLarge++
				result.addRetained(path, retainedByContentSize, keepPreview)
				directories.blockEntry(path)
				return nil
			}
			result.contentChecked++
			if contentMatched {
				result.contentExcluded++
				result.addRetained(path, retainedByContent, keepPreview)
				directories.blockEntry(path)
				return nil
			}
		}

		target, targetErr := flattenTarget(root, path)
		if targetErr != nil {
			result.errors = append(result.errors, operationError{path: path, err: targetErr})
			directories.blockEntry(path)
			return nil
		}
		file := candidate{path: path, target: target, targetBase: target, size: info.Size(), pattern: pattern}
		provisional = append(provisional, file)
		return nil
	})

	for _, file := range provisional {
		plannedTarget, numbered, ok := reserveFlattenTarget(file.target, occupied)
		if !ok {
			result.tooLong++
			result.addRetained(file.path, retainedFlattenTooLong, keepPreview)
			directories.blockEntry(file.path)
			continue
		}
		file.target = plannedTarget
		if numbered {
			result.numbered++
		}
		result.files = append(result.files, file)
		if file.size == 0 {
			result.zeroSize++
		}
		stats := result.byPattern[file.pattern]
		stats.count++
		stats.size += file.size
		result.byPattern[file.pattern] = stats
		directories.removeEntry(file.path)
	}
	result.emptyDirs = directories.emptyDirectories()
	display.scan(result.visited, int64(len(result.files)), invert, true)
	return result
}

func flattenTarget(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if relative == "." || relative == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径不在扫描根目录内")
	}
	name := strings.ReplaceAll(relative, string(filepath.Separator), flattenSeparator)
	return filepath.Join(root, name), nil
}

func flattenCollisionKey(name string) string {
	return strings.ToLower(name)
}

func reserveFlattenTarget(desired string, occupied map[string]struct{}) (target string, numbered, ok bool) {
	directory := filepath.Dir(desired)
	name := filepath.Base(desired)
	if windowsFilenameUnits(name) > 255 {
		return "", false, false
	}
	key := flattenCollisionKey(name)
	if _, exists := occupied[key]; !exists {
		occupied[key] = struct{}{}
		return desired, false, true
	}

	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	if stem == "" {
		stem = name
		extension = ""
	}
	for sequence := uint64(2); sequence > 0; sequence++ {
		candidateName := fmt.Sprintf("%s (%d)%s", stem, sequence, extension)
		if windowsFilenameUnits(candidateName) > 255 {
			return "", true, false
		}
		key = flattenCollisionKey(candidateName)
		if _, exists := occupied[key]; exists {
			continue
		}
		occupied[key] = struct{}{}
		return filepath.Join(directory, candidateName), true, true
	}
	return "", true, false
}

func windowsFilenameUnits(name string) int {
	return len(utf16.Encode([]rune(name)))
}

func (result *scanResult) addRetained(path, reason string, limit int) {
	result.retainedTotal++
	if limit > 0 && len(result.retained) < limit {
		result.retained = append(result.retained, retainedItem{path: path, reason: reason})
	}
}

func newDirectoryTracker(root string, enabled bool) *directoryTracker {
	tracker := &directoryTracker{enabled: enabled, root: root}
	if enabled {
		tracker.states = make(map[string]*directoryState)
		tracker.add(root)
	}
	return tracker
}

func (tracker *directoryTracker) add(path string) {
	if !tracker.enabled {
		return
	}
	if _, exists := tracker.states[path]; exists {
		return
	}
	tracker.states[path] = &directoryState{}
	tracker.order = append(tracker.order, path)
}

func (tracker *directoryTracker) blockEntry(path string) {
	if !tracker.enabled || path == tracker.root {
		return
	}
	if parent, exists := tracker.states[filepath.Dir(path)]; exists {
		parent.blocked = true
	}
}

func (tracker *directoryTracker) removeEntry(path string) {
	if !tracker.enabled || path == tracker.root {
		return
	}
	if parent, exists := tracker.states[filepath.Dir(path)]; exists {
		parent.changes = true
	}
}

func (tracker *directoryTracker) blockForError(path string) {
	if !tracker.enabled {
		return
	}
	if directory, exists := tracker.states[path]; exists {
		directory.blocked = true
		return
	}
	tracker.blockEntry(path)
}

func (tracker *directoryTracker) emptyDirectories() []emptyDirectory {
	if !tracker.enabled {
		return nil
	}

	emptyDirs := make([]emptyDirectory, 0)
	for i := len(tracker.order) - 1; i >= 0; i-- {
		path := tracker.order[i]
		if path == tracker.root {
			continue
		}
		state := tracker.states[path]
		parent := tracker.states[filepath.Dir(path)]
		if state.blocked {
			if parent != nil {
				parent.blocked = true
			}
			continue
		}
		emptyDirs = append(emptyDirs, emptyDirectory{path: path, afterProcessing: state.changes})
		if parent != nil {
			parent.changes = true
		}
	}
	return emptyDirs
}

func isExcludedPath(root, path string, exclusions []string) bool {
	if len(exclusions) == 0 {
		return false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return false
	}
	relative = strings.ToLower(filepath.Clean(relative))
	for _, keyword := range exclusions {
		if strings.Contains(relative, keyword) {
			return true
		}
	}
	return false
}

func printScanSummary(out io.Writer, result scanResult, preview, emptyDirPreview int, invert bool) {
	var totalSize int64
	for _, stats := range result.byPattern {
		totalSize += stats.size
	}
	fmt.Fprintln(out, "扫描完成")
	fmt.Fprintf(out, "  扫描项目: %s\n", formatInteger(result.visited))
	if invert {
		fmt.Fprintf(out, "  保留命中: %s\n", formatInteger(result.kept))
		fmt.Fprintf(out, "  待处理文件: %s\n", formatInteger(int64(len(result.files))))
	} else {
		fmt.Fprintf(out, "  匹配文件: %s\n", formatInteger(int64(len(result.files))))
	}
	if invert {
		fmt.Fprintf(out, "  待处理大小: %s\n", formatBytes(totalSize))
	} else {
		fmt.Fprintf(out, "  匹配大小: %s\n", formatBytes(totalSize))
	}
	fmt.Fprintf(out, "  0 字节项: %s\n", formatInteger(result.zeroSize))
	fmt.Fprintf(out, "  链接/重解析点: %s\n", formatInteger(result.linkLikes))
	if result.excluded > 0 {
		fmt.Fprintf(out, "  排除命中: %s（目录 %s；目录内容未继续扫描）\n", formatInteger(result.excluded), formatInteger(result.excludedDirs))
	}
	if result.contentFilterEnabled {
		fmt.Fprintf(out, "  内容检查: %s 个（读取 %s）\n", formatInteger(result.contentChecked), formatBytes(result.contentBytes))
		fmt.Fprintf(out, "  内容关键词排除: %s 个\n", formatInteger(result.contentExcluded))
		if result.contentTooLarge > 0 {
			fmt.Fprintf(out, "  超过内容上限并保留: %s 个\n", formatInteger(result.contentTooLarge))
		}
	}
	if result.unsupported > 0 {
		fmt.Fprintf(out, "  不支持的特殊类型并保留: %s 个\n", formatInteger(result.unsupported))
	}
	if result.emptyDirs != nil {
		fmt.Fprintf(out, "  空文件夹候选: %s 个\n", formatInteger(int64(len(result.emptyDirs))))
	}

	keys := make([]string, 0, len(result.byPattern))
	for pattern := range result.byPattern {
		keys = append(keys, pattern)
	}
	sort.Strings(keys)
	for _, pattern := range keys {
		stats := result.byPattern[pattern]
		fmt.Fprintf(out, "    %-12s %12s 个  %10s\n", displayPattern(pattern), formatInteger(stats.count), formatBytes(stats.size))
	}

	shown := preview
	if shown > len(result.files) {
		shown = len(result.files)
	}
	if shown > 0 {
		fmt.Fprintf(out, "预览（前 %d 个）:\n", shown)
		for i := 0; i < shown; i++ {
			labels := make([]string, 0, 2)
			if result.files[i].isLinkLike {
				labels = append(labels, "链接/重解析点")
			}
			if result.files[i].size == 0 {
				labels = append(labels, "0 B")
			}
			label := ""
			if len(labels) > 0 {
				label = "[" + strings.Join(labels, ", ") + "] "
			}
			fmt.Fprintf(out, "  %s%s\n", label, result.files[i].path)
		}
		if shown < len(result.files) {
			fmt.Fprintf(out, "  ... 另有 %s 个\n", formatInteger(int64(len(result.files)-shown)))
		}
	}
	if len(result.retained) > 0 {
		fmt.Fprintf(out, "保留预览（前 %d 个）:\n", len(result.retained))
		for _, item := range result.retained {
			fmt.Fprintf(out, "  [%s] %s\n", item.reason, item.path)
		}
		if int64(len(result.retained)) < result.retainedTotal {
			fmt.Fprintf(out, "  ... 另有 %s 个显式保留项\n", formatInteger(result.retainedTotal-int64(len(result.retained))))
		}
	}
	emptyShown := emptyDirPreview
	if emptyShown > len(result.emptyDirs) {
		emptyShown = len(result.emptyDirs)
	}
	if emptyShown > 0 {
		fmt.Fprintf(out, "空文件夹预览（前 %d 个）:\n", emptyShown)
		for i := 0; i < emptyShown; i++ {
			state := "当前为空"
			if result.emptyDirs[i].afterProcessing {
				state = "处理后为空"
			}
			fmt.Fprintf(out, "  [%s] %s\n", state, result.emptyDirs[i].path)
		}
		if emptyShown < len(result.emptyDirs) {
			fmt.Fprintf(out, "  ... 另有 %s 个空文件夹候选\n", formatInteger(int64(len(result.emptyDirs)-emptyShown)))
		}
	}
}

func printFlattenSummary(out io.Writer, result flattenScanResult, preview, emptyDirPreview int) {
	var totalSize int64
	for _, stats := range result.byPattern {
		totalSize += stats.size
	}
	fmt.Fprintln(out, "展平扫描完成")
	fmt.Fprintf(out, "  扫描项目: %s\n", formatInteger(result.visited))
	fmt.Fprintf(out, "  待移动文件: %s\n", formatInteger(int64(len(result.files))))
	fmt.Fprintf(out, "  待移动大小: %s\n", formatBytes(totalSize))
	fmt.Fprintf(out, "  0 字节文件: %s\n", formatInteger(result.zeroSize))
	if result.rootFiles > 0 {
		fmt.Fprintf(out, "  已在根目录并保留: %s 个\n", formatInteger(result.rootFiles))
	}
	if result.kept > 0 {
		fmt.Fprintf(out, "  类型筛选保留: %s 个\n", formatInteger(result.kept))
	}
	if result.linksRetained > 0 {
		fmt.Fprintf(out, "  链接/重解析点保留: %s 个\n", formatInteger(result.linksRetained))
	}
	if result.numbered > 0 {
		fmt.Fprintf(out, "  重复名称自动编号: %s 个\n", formatInteger(result.numbered))
	}
	if result.tooLong > 0 {
		fmt.Fprintf(out, "  文件名过长并保留: %s 个\n", formatInteger(result.tooLong))
	}
	if result.excluded > 0 {
		fmt.Fprintf(out, "  排除命中: %s（目录 %s；目录内容未继续扫描）\n", formatInteger(result.excluded), formatInteger(result.excludedDirs))
	}
	if result.contentFilterEnabled {
		fmt.Fprintf(out, "  内容检查: %s 个（读取 %s）\n", formatInteger(result.contentChecked), formatBytes(result.contentBytes))
		fmt.Fprintf(out, "  内容关键词排除: %s 个\n", formatInteger(result.contentExcluded))
		if result.contentTooLarge > 0 {
			fmt.Fprintf(out, "  超过内容上限并保留: %s 个\n", formatInteger(result.contentTooLarge))
		}
	}
	if result.unsupported > 0 {
		fmt.Fprintf(out, "  不支持的特殊类型并保留: %s 个\n", formatInteger(result.unsupported))
	}
	if result.emptyDirs != nil {
		fmt.Fprintf(out, "  空文件夹候选: %s 个\n", formatInteger(int64(len(result.emptyDirs))))
	}

	keys := make([]string, 0, len(result.byPattern))
	for pattern := range result.byPattern {
		keys = append(keys, pattern)
	}
	sort.Strings(keys)
	for _, pattern := range keys {
		stats := result.byPattern[pattern]
		fmt.Fprintf(out, "    %-12s %12s 个  %10s\n", displayPattern(pattern), formatInteger(stats.count), formatBytes(stats.size))
	}

	shown := preview
	if shown > len(result.files) {
		shown = len(result.files)
	}
	if shown > 0 {
		fmt.Fprintf(out, "移动预览（前 %d 个）:\n", shown)
		for i := 0; i < shown; i++ {
			label := ""
			if result.files[i].size == 0 {
				label = "[0 B] "
			}
			fmt.Fprintf(out, "  %s%s\n", label, result.files[i].path)
			fmt.Fprintf(out, "    -> %s\n", result.files[i].target)
		}
		if shown < len(result.files) {
			fmt.Fprintf(out, "  ... 另有 %s 个\n", formatInteger(int64(len(result.files)-shown)))
		}
	}
	if len(result.retained) > 0 {
		fmt.Fprintf(out, "保留预览（前 %d 个）:\n", len(result.retained))
		for _, item := range result.retained {
			fmt.Fprintf(out, "  [%s] %s\n", item.reason, item.path)
		}
		if int64(len(result.retained)) < result.retainedTotal {
			fmt.Fprintf(out, "  ... 另有 %s 个显式保留项\n", formatInteger(result.retainedTotal-int64(len(result.retained))))
		}
	}
	emptyShown := emptyDirPreview
	if emptyShown > len(result.emptyDirs) {
		emptyShown = len(result.emptyDirs)
	}
	if emptyShown > 0 {
		fmt.Fprintf(out, "空文件夹预览（前 %d 个）:\n", emptyShown)
		for i := 0; i < emptyShown; i++ {
			state := "当前为空"
			if result.emptyDirs[i].afterProcessing {
				state = "处理后为空"
			}
			fmt.Fprintf(out, "  [%s] %s\n", state, result.emptyDirs[i].path)
		}
		if emptyShown < len(result.emptyDirs) {
			fmt.Fprintf(out, "  ... 另有 %s 个空文件夹候选\n", formatInteger(int64(len(result.emptyDirs)-emptyShown)))
		}
	}
}

func displayPattern(pattern string) string {
	if pattern == "" {
		return "<无扩展名>"
	}
	return pattern
}

func displayPatterns(patterns []string) string {
	labels := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		labels = append(labels, displayPattern(pattern))
	}
	sort.Strings(labels)
	return strings.Join(labels, ", ")
}

func actualExtension(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

func confirmDeletion(in io.Reader, out io.Writer, fileCount, directoryCount int, root string, invert bool) bool {
	if invert {
		fmt.Fprintln(out, "警告：反向模式已启用，保留类型之外的文件将被永久删除。")
	}
	fmt.Fprintf(out, "即将从 %s 永久删除 %s 个文件和 %s 个空文件夹。\n", root, formatInteger(int64(fileCount)), formatInteger(int64(directoryCount)))
	fmt.Fprint(out, "输入 DELETE 确认: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	return strings.TrimSpace(line) == "DELETE"
}

func confirmFlatten(in io.Reader, out io.Writer, fileCount, directoryCount int, root string) bool {
	fmt.Fprintf(out, "即将把 %s 中 %s 个文件移动到根目录，并删除 %s 个空文件夹。\n", root, formatInteger(int64(fileCount)), formatInteger(int64(directoryCount)))
	fmt.Fprint(out, "输入 MOVE 确认: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	return strings.TrimSpace(line) == "MOVE"
}

func deleteCandidates(files []candidate, patterns []string, invert bool, filter contentFilter, workers int, logWriter io.Writer, progress io.Writer) (deleteSummary, []operationError) {
	jobs := make(chan candidate)
	results := make(chan deleteResult)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for file := range jobs {
			info, err := os.Lstat(file.path)
			if err != nil {
				if os.IsNotExist(err) {
					results <- deleteResult{file: file, status: "SKIPPED", reason: "文件已不存在"}
				} else {
					results <- deleteResult{file: file, status: "ERROR", err: err}
				}
				continue
			}
			_, stillMatches := matchPattern(filepath.Base(file.path), patterns)
			selected := stillMatches
			if invert {
				selected = !stillMatches
			}
			isLinkLike, supported := classifyFileInfo(info)
			if !supported || !selected {
				results <- deleteResult{file: file, status: "SKIPPED", reason: "类型或扩展名已变化"}
				continue
			}
			file.size = info.Size()
			file.isLinkLike = isLinkLike
			if filter.enabled() && !isLinkLike {
				contentMatched, tooLarge, _, err := filter.match(file.path, info.Size())
				if err != nil {
					results <- deleteResult{file: file, status: "ERROR", err: fmt.Errorf("删除前内容复查: %w", err)}
					continue
				}
				if tooLarge {
					results <- deleteResult{file: file, status: "SKIPPED", reason: "文件超过内容检查上限"}
					continue
				}
				if contentMatched {
					results <- deleteResult{file: file, status: "SKIPPED", reason: "内容命中排除关键词"}
					continue
				}
			}
			if err := os.Remove(file.path); err != nil {
				results <- deleteResult{file: file, status: "ERROR", err: err}
				continue
			}
			results <- deleteResult{file: file, status: "DELETED"}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	go func() {
		for _, file := range files {
			jobs <- file
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var summary deleteSummary
	var errors []operationError
	var processed int64
	display := newProgressDisplay(progress)
	for result := range results {
		processed++
		switch result.status {
		case "DELETED":
			summary.deleted++
			summary.deletedSize += result.file.size
			if result.file.isLinkLike {
				summary.linkLikes++
			}
		case "SKIPPED":
			summary.skipped++
		case "ERROR":
			summary.failed++
			errors = append(errors, operationError{path: result.file.path, err: result.err})
		}
		if logWriter != nil {
			if result.err != nil {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\t%v\n", result.status, result.file.size, result.file.path, result.err)
			} else if result.reason != "" {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\t%s\n", result.status, result.file.size, result.file.path, result.reason)
			} else {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\n", result.status, result.file.size, result.file.path)
			}
		}
		display.delete(processed, int64(len(files)), summary.deleted, summary.failed, false)
	}
	display.delete(processed, int64(len(files)), summary.deleted, summary.failed, true)
	return summary, errors
}

func moveCandidates(files []candidate, root string, patterns []string, hasSelection, invert bool, filter contentFilter, workers int, logWriter io.Writer, progress io.Writer) (moveSummary, []operationError) {
	jobs := make(chan candidate)
	results := make(chan moveResult)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for file := range jobs {
			info, err := os.Lstat(file.path)
			if err != nil {
				if os.IsNotExist(err) {
					results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "源文件已不存在"}
				} else {
					results <- moveResult{file: file, status: "MOVE_ERROR", err: err}
				}
				continue
			}

			selected := true
			if hasSelection {
				_, matched := matchPattern(filepath.Base(file.path), patterns)
				selected = matched
				if invert {
					selected = !matched
				}
			}
			isLinkLike, supported := classifyFileInfo(info)
			if !selected || !supported || isLinkLike || !info.Mode().IsRegular() {
				results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "类型或扩展名已变化"}
				continue
			}
			file.size = info.Size()
			if filter.enabled() {
				contentMatched, tooLarge, _, matchErr := filter.match(file.path, info.Size())
				if matchErr != nil {
					results <- moveResult{file: file, status: "MOVE_ERROR", err: fmt.Errorf("移动前内容复查: %w", matchErr)}
					continue
				}
				if tooLarge {
					results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "文件超过内容检查上限"}
					continue
				}
				if contentMatched {
					results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "内容命中排除关键词"}
					continue
				}
			}

			expectedTarget, targetErr := flattenTarget(root, file.path)
			if targetErr != nil || filepath.Clean(expectedTarget) != filepath.Clean(file.targetBase) || filepath.Dir(file.target) != root {
				if targetErr == nil {
					targetErr = fmt.Errorf("目标路径已不符合展平计划")
				}
				results <- moveResult{file: file, status: "MOVE_ERROR", err: targetErr}
				continue
			}
			if _, targetErr = os.Lstat(file.target); targetErr == nil {
				results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "目标已存在，未覆盖"}
				continue
			} else if !os.IsNotExist(targetErr) {
				results <- moveResult{file: file, status: "MOVE_ERROR", err: fmt.Errorf("检查目标: %w", targetErr)}
				continue
			}
			if linkErr := os.Link(file.path, file.target); linkErr != nil {
				if os.IsExist(linkErr) {
					results <- moveResult{file: file, status: "MOVE_SKIPPED", reason: "目标已存在，未覆盖"}
				} else {
					results <- moveResult{file: file, status: "MOVE_ERROR", err: fmt.Errorf("创建目标: %w", linkErr)}
				}
				continue
			}

			targetInfo, targetErr := os.Lstat(file.target)
			sourceInfo, sourceErr := os.Lstat(file.path)
			if targetErr != nil || sourceErr != nil || !os.SameFile(sourceInfo, targetInfo) {
				verificationErr := fmt.Errorf("无法验证新旧路径指向同一文件")
				if targetErr != nil {
					verificationErr = fmt.Errorf("验证目标: %w", targetErr)
				} else if sourceErr != nil {
					verificationErr = fmt.Errorf("验证源文件: %w", sourceErr)
				}
				results <- moveResult{file: file, status: "MOVE_ERROR", err: verificationErr}
				continue
			}
			if removeErr := os.Remove(file.path); removeErr != nil {
				rollbackErr := os.Remove(file.target)
				if rollbackErr != nil {
					removeErr = fmt.Errorf("删除源路径失败: %v；回滚目标也失败: %w", removeErr, rollbackErr)
				} else {
					removeErr = fmt.Errorf("删除源路径失败，已回滚目标: %w", removeErr)
				}
				results <- moveResult{file: file, status: "MOVE_ERROR", err: removeErr}
				continue
			}
			results <- moveResult{file: file, status: "MOVED"}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	go func() {
		for _, file := range files {
			jobs <- file
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var summary moveSummary
	var moveErrors []operationError
	var processed int64
	display := newProgressDisplay(progress)
	for result := range results {
		processed++
		switch result.status {
		case "MOVED":
			summary.moved++
			summary.movedSize += result.file.size
		case "MOVE_SKIPPED":
			summary.skipped++
		case "MOVE_ERROR":
			summary.failed++
			moveErrors = append(moveErrors, operationError{path: result.file.path, err: result.err})
		}
		if logWriter != nil {
			if result.err != nil {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\t%s\t%v\n", result.status, result.file.size, result.file.path, result.file.target, result.err)
			} else if result.reason != "" {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\t%s\t%s\n", result.status, result.file.size, result.file.path, result.file.target, result.reason)
			} else {
				fmt.Fprintf(logWriter, "%s\t%d\t%s\t%s\n", result.status, result.file.size, result.file.path, result.file.target)
			}
		}
		display.move(processed, int64(len(files)), summary.moved, summary.failed, false)
	}
	display.move(processed, int64(len(files)), summary.moved, summary.failed, true)
	return summary, moveErrors
}

func deleteEmptyDirectories(directories []emptyDirectory, logWriter io.Writer, progress io.Writer) (directoryDeleteSummary, []operationError) {
	var summary directoryDeleteSummary
	var errors []operationError
	display := newProgressDisplay(progress)
	for index, directory := range directories {
		status := "DIR_SKIPPED"
		reason := ""
		info, err := os.Lstat(directory.path)
		if err != nil {
			if os.IsNotExist(err) {
				summary.skipped++
				reason = "文件夹已不存在"
			} else {
				summary.failed++
				status = "DIR_ERROR"
				errors = append(errors, operationError{path: directory.path, err: err})
			}
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			summary.skipped++
			reason = "路径已不再是普通文件夹"
		} else {
			entries, readErr := os.ReadDir(directory.path)
			switch {
			case readErr != nil:
				summary.failed++
				status = "DIR_ERROR"
				err = readErr
				errors = append(errors, operationError{path: directory.path, err: readErr})
			case len(entries) > 0:
				summary.skipped++
				reason = "文件夹不是空的"
			default:
				if removeErr := os.Remove(directory.path); removeErr != nil {
					summary.failed++
					status = "DIR_ERROR"
					err = removeErr
					errors = append(errors, operationError{path: directory.path, err: removeErr})
				} else {
					summary.deleted++
					status = "DIR_DELETED"
				}
			}
		}

		if logWriter != nil {
			if err != nil {
				fmt.Fprintf(logWriter, "%s\t%s\t%v\n", status, directory.path, err)
			} else if reason != "" {
				fmt.Fprintf(logWriter, "%s\t%s\t%s\n", status, directory.path, reason)
			} else {
				fmt.Fprintf(logWriter, "%s\t%s\n", status, directory.path)
			}
		}
		display.directories(int64(index+1), int64(len(directories)), summary.deleted, summary.failed, false)
	}
	display.directories(int64(len(directories)), int64(len(directories)), summary.deleted, summary.failed, true)
	return summary, errors
}

func classifyFileInfo(info fs.FileInfo) (isLinkLike, supported bool) {
	mode := info.Mode()
	isLinkLike = mode&os.ModeSymlink != 0 || mode&os.ModeIrregular != 0 && isReparsePoint(info)
	return isLinkLike, mode.IsRegular() || isLinkLike
}

func newProgressDisplay(out io.Writer) *progressDisplay {
	display := &progressDisplay{out: out}
	if reporter, ok := out.(progressReporter); ok {
		display.reporter = reporter
	}
	if file, ok := out.(*os.File); ok {
		if info, err := file.Stat(); err == nil {
			display.interactive = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return display
}

func (display *progressDisplay) scan(visited, selected int64, invert, done bool) {
	display.report(progressState{phase: "scan", processed: visited, selected: selected, done: done})
	if !display.shouldRender(done) {
		return
	}
	const width = 30
	bar := strings.Repeat("=", width)
	state := "完成"
	if !done {
		cells := []byte(strings.Repeat("-", width))
		const markerWidth = 5
		start := int((visited/250)%int64(width+markerWidth)) - markerWidth
		for i := 0; i < markerWidth; i++ {
			position := start + i
			if position >= 0 && position < width {
				cells[position] = '='
			}
		}
		bar = string(cells)
		state = "扫描中"
	}
	selectionLabel := "匹配"
	if invert {
		selectionLabel = "待处理"
	}
	display.render(fmt.Sprintf("扫描 [%s] %s | 已扫描 %s | %s %s", bar, state, formatInteger(visited), selectionLabel, formatInteger(selected)), done)
}

func (display *progressDisplay) delete(processed, total, deleted, failed int64, done bool) {
	display.report(progressState{phase: "delete", processed: processed, total: total, succeeded: deleted, failed: failed, done: done})
	if !display.shouldRender(done) {
		return
	}
	const width = 30
	percent := int64(100)
	filled := width
	if total > 0 {
		percent = processed * 100 / total
		filled = int(processed * width / total)
	}
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
	display.render(fmt.Sprintf("处理 [%s] %3d%% | %s/%s | 删除 %s | 失败 %s", bar, percent, formatInteger(processed), formatInteger(total), formatInteger(deleted), formatInteger(failed)), done)
}

func (display *progressDisplay) move(processed, total, moved, failed int64, done bool) {
	display.report(progressState{phase: "move", processed: processed, total: total, succeeded: moved, failed: failed, done: done})
	if !display.shouldRender(done) {
		return
	}
	const width = 30
	percent := int64(100)
	filled := width
	if total > 0 {
		percent = processed * 100 / total
		filled = int(processed * width / total)
	}
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
	display.render(fmt.Sprintf("移动 [%s] %3d%% | %s/%s | 成功 %s | 失败 %s", bar, percent, formatInteger(processed), formatInteger(total), formatInteger(moved), formatInteger(failed)), done)
}

func (display *progressDisplay) directories(processed, total, deleted, failed int64, done bool) {
	display.report(progressState{phase: "directories", processed: processed, total: total, succeeded: deleted, failed: failed, done: done})
	if !display.shouldRender(done) {
		return
	}
	const width = 30
	percent := int64(100)
	filled := width
	if total > 0 {
		percent = processed * 100 / total
		filled = int(processed * width / total)
	}
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
	display.render(fmt.Sprintf("空目录 [%s] %3d%% | %s/%s | 删除 %s | 失败 %s", bar, percent, formatInteger(processed), formatInteger(total), formatInteger(deleted), formatInteger(failed)), done)
}

func (display *progressDisplay) report(state progressState) {
	if display.reporter == nil {
		return
	}
	if !state.done && time.Since(display.lastReport) < 100*time.Millisecond {
		return
	}
	display.lastReport = time.Now()
	display.reporter.reportProgress(state)
}

func (display *progressDisplay) shouldRender(force bool) bool {
	if display.out == nil {
		return false
	}
	if force {
		return true
	}
	if !display.interactive || time.Since(display.lastUpdate) < 100*time.Millisecond {
		return false
	}
	display.lastUpdate = time.Now()
	return true
}

func (display *progressDisplay) render(line string, done bool) {
	if display.interactive {
		fmt.Fprintf(display.out, "\r%s        ", line)
		if done {
			fmt.Fprintln(display.out)
		}
		return
	}
	if done {
		fmt.Fprintln(display.out, line)
	}
}

func printErrors(out io.Writer, title string, errors []operationError, limit int) {
	if len(errors) == 0 {
		return
	}
	fmt.Fprintf(out, "%s: %s 个\n", title, formatInteger(int64(len(errors))))
	shown := len(errors)
	if shown > limit {
		shown = limit
	}
	for i := 0; i < shown; i++ {
		fmt.Fprintf(out, "  %s: %v\n", errors[i].path, errors[i].err)
	}
	if shown < len(errors) {
		fmt.Fprintf(out, "  ... 另有 %s 个错误未显示\n", formatInteger(int64(len(errors)-shown)))
	}
}

func formatInteger(value int64) string {
	negative := value < 0
	if negative {
		value = -value
	}
	digits := fmt.Sprintf("%d", value)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	if negative {
		return "-" + digits
	}
	return digits
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor := int64(unit)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	unitIndex := 0
	for size/divisor >= unit && unitIndex < len(units)-1 {
		divisor *= unit
		unitIndex++
	}
	return fmt.Sprintf("%.1f %s", float64(size)/float64(divisor), units[unitIndex])
}
