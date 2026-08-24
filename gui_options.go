package main

import "strings"

const (
	guiModeDelete  = "删除"
	guiModeFlatten = "展平"
)

type guiOptions struct {
	root              string
	mode              string
	extensions        string
	exclusions        string
	contentExclusions string
	contentMaxMiB     string
	workers           string
	preview           string
	keepPreview       string
	emptyDirPreview   string
	logPath           string
	noExtension       bool
	invert            bool
	emptyDirs         bool
	ignoreScanErrors  bool
}

func (options guiOptions) arguments(execute bool) []string {
	args := make([]string, 0, 30)
	appendValue := func(flag, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			args = append(args, flag, value)
		}
	}

	appendValue("--root", options.root)
	appendValue("--ext", options.extensions)
	appendValue("--exclude", options.exclusions)
	appendValue("--exclude-content", options.contentExclusions)
	appendValue("--content-max-mib", options.contentMaxMiB)
	appendValue("--workers", options.workers)
	appendValue("--preview", options.preview)
	appendValue("--keep-preview", options.keepPreview)
	appendValue("--empty-dir-preview", options.emptyDirPreview)
	appendValue("--log", options.logPath)
	if options.noExtension {
		args = append(args, "--no-ext")
	}
	if options.invert {
		args = append(args, "--invert")
	}
	if options.emptyDirs {
		args = append(args, "--empty-dirs")
	}
	if options.ignoreScanErrors {
		args = append(args, "--ignore-scan-errors")
	}
	if options.mode == guiModeFlatten {
		args = append(args, "--flatten")
		if execute {
			args = append(args, "--apply", "--yes")
		}
	} else if execute {
		args = append(args, "--delete", "--yes")
	}
	return args
}

func (options guiOptions) fingerprint() string {
	return strings.Join(options.arguments(false), "\x00")
}
