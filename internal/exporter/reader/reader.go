package reader

import (
	"fmt"
	"ops-exporter/internal/exporter/model"
	"os"
	"path/filepath"
	"strings"
)

// FileError 表示某个任务结果文件的读取或解析失败。
// 保留文件路径，方便日志定位到具体是哪个任务文件出了问题。
type FileError struct {
	Path string
	Err  error
}

func (e FileError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

// Unwrap 让调用方可以用 errors.Is / errors.As 判断底层原因。
func (e FileError) Unwrap() error {
	return e.Err
}

// LoadTaskResults 读取 dir 下所有 .json 文件并解析为任务结果。
//
// 返回值有三个，对应三种不同性质的失败：
//   - results：解析成功的任务，按文件名排序（os.ReadDir 本身有序）
//   - fileErrs：单个文件的失败原因（读不出来或内容非法），
//     由调用方决定是否记日志；这类错误不影响其他文件
//   - err：目录级失败（目录不存在、无读权限）。
//     此时 results 和 fileErrs 都为空，属于需要人工介入的问题
//
// 注意：任务执行失败（status=failed）是合法数据，会出现在 results 里，
// 不会出现在 fileErrs 里 —— 前者是"任务没跑成功"，后者是"结果读不出来"。
func LoadTaskResults(dir string) ([]model.TaskResult, []FileError, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read task result dir %q: %w", dir, err)
	}

	var results []model.TaskResult
	var fileErrs []FileError

	for _, entry := range entries {
		// 只处理普通 .json 文件，子目录和其他类型的文件静默跳过。
		if entry.IsDir() || !isJSONFile(entry.Name()) {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			fileErrs = append(fileErrs, FileError{Path: path, Err: fmt.Errorf("read file: %w", err)})
			continue
		}

		result, err := model.ParseTaskResult(data)
		if err != nil {
			fileErrs = append(fileErrs, FileError{Path: path, Err: fmt.Errorf("parse: %w", err)})
			continue
		}

		results = append(results, result)
	}

	return results, fileErrs, nil
}

// isJSONFile 判断文件名是否为 .json（大小写不敏感）。
// 大小写不敏感是因为 Windows 上文件扩展名常见 .JSON，没必要因此漏采。
func isJSONFile(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".json")
}
