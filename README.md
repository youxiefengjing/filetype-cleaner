# filetype-cleaner

面向大量小文件的安全整理工具，提供 Go CLI 和 Windows GUI 两个入口。

按规则清理不需要的文件，以及把分散在多级目录中的文件集中到根目录。

## 能力概览

| 能力 | CLI | Windows GUI |
| --- | --- | --- |
| 按扩展名处理 | 支持 | 支持 |
| 无扩展名文件 | `--no-ext` | 复选框 |
| 反向模式 | `--invert` | 复选框 |
| 路径 / 内容排除 | `--exclude` / `--exclude-content` | 规则输入框 |
| 展平目录 | `--flatten` | 展平模式 |
| 空目录处理 | `--empty-dirs` | 复选框 |
| 进度、预览、日志 | 支持 | 支持 |

## 两种工作模式

### 删除

选择扩展名后，工具扫描并列出候选文件。`--delete` 才会进入实际删除流程；未指定时始终是预览。

```powershell
.\filetype-cleaner.exe --root "D:\data" --ext ".tmp,.log"
.\filetype-cleaner.exe --root "D:\data" --ext ".tmp,.log" --delete
```

### 展平

`--flatten` 将子目录中的普通文件映射到扫描根目录，并把原相对路径编码到文件名中：

```text
a\b\file.txt  ->  a__b__file.txt
```

展平同样默认只预览，`--apply` 才会移动文件。重复目标名自动编号，并且不会覆盖已有文件：

```text
a__b__file.txt
a__b__file (2).txt
a__b__file (3).txt
```

```powershell
.\filetype-cleaner.exe --root "D:\data" --flatten
.\filetype-cleaner.exe --root "D:\data" --flatten --apply --empty-dirs --yes
```

## 安全模型

- 删除和展平都默认预览，执行前要求显式参数和确认。
- 执行阶段会重新检查文件类型、内容排除规则和目标路径。
- 目标文件已存在时不会覆盖；重复名称使用 `(2)`、`(3)` 等序号。
- 根目录文件保持不变；符号链接和链接型重解析点不参与展平，删除时只处理链接本身。
- 扫描错误默认会取消实际处理；可见的保留项会显示原因。
- 空目录在文件操作后从深到浅重新检查，扫描根目录永不删除。
- 拒绝直接对 `C:\`、`D:\` 等磁盘根目录执行删除或展平。
- 删除是永久操作，不进入回收站。

进度按文件数量计算，因此 0 字节文件也会正常统计和推进。

## Windows GUI

GUI 可执行文件为 `filetype-cleaner-gui.exe`。它使用与 CLI 相同的扫描和处理逻辑，提供目录选择、规则输入、模式切换、预览、确认对话框、后台执行和结果输出。

界面约束是“先预览，再执行”：只要设置发生变化，执行按钮就会重新锁定。GUI 版本构建于 `feature/gui` 分支。

## 核心参数

完整参数可运行 `filetype-cleaner.exe --help` 查看。常用参数如下：

| 参数 | 作用 |
| --- | --- |
| `--root PATH` | 扫描根目录，必填 |
| `--ext LIST` | 扩展名列表，例如 `.c,.h` |
| `--no-ext` | 选择无扩展名文件 |
| `--invert` | 保留指定类型，处理其他类型 |
| `--exclude LIST` | 按相对路径关键词排除 |
| `--exclude-content LIST` | 按文件内容关键词排除 |
| `--flatten` | 展平模式 |
| `--apply` | 执行展平移动，必须配合 `--flatten` |
| `--delete` | 执行删除 |
| `--empty-dirs` | 同时处理空目录 |
| `--workers N` | 并发数，范围 1-64 |
| `--log PATH` | 追加处理日志 |

扩展名不区分大小写，支持 `.c`、`c`、`*.c` 和多段扩展名。`--yes` 仅跳过确认，不会跳过扫描和执行阶段的安全复查。

## 构建

需要 Go 1.22 或更高版本。

CLI：

```powershell
go test ./...
go vet ./...
go build -buildvcs=false -trimpath -ldflags "-s -w" -o filetype-cleaner.exe .
```

GUI 使用 Fyne v2，需要 CGO 和 C 编译器（Windows 可使用 MinGW-w64）：

```powershell
go test -tags gui ./...
go vet -tags gui ./...
go build -tags gui -buildvcs=false -trimpath `
  -ldflags "-s -w -H=windowsgui" `
  -o filetype-cleaner-gui.exe .
```

GUI 依赖由 `go.mod` 和 `go.sum` 锁定；构建产物已通过 `.gitignore` 排除。
