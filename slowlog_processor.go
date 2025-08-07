package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type SlowLogResult struct {
	Count        int     `json:"count"`
	Time         float64 `json:"time_seconds"`
	LockTime     float64 `json:"lock_time_seconds"`
	RowsSent     float64 `json:"rows_sent"`
	RowsExamined float64 `json:"rows_examined`
	User         string  `json:"user"`
	Host         string  `json:"host"`
	Query        string  `json:"query"`
}

func handleSlowlog(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 获取慢日志路径
		slowLogPath, err := getSlowLogPath(logger)
		if err != nil {
			logger.Error("Failed to get slow log path", "err", err)
			http.Error(w, fmt.Sprintf("Error getting slow log path: %v", err), http.StatusInternalServerError)
			return
		}
		//slowLogPath = "/usr/local/work/mysqld_exporter/mysql-slow.log"
		// 2. 检查文件是否存在且可读
		if err := checkFileAccess(slowLogPath); err != nil {
			logger.Error("File access error", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// 3. 计算24小时前的时间并生成grep过滤条件
		now := time.Now()
		twentyFourHoursAgo := now.Add(-24 * time.Hour)
		timePatterns := generateTimePatterns(twentyFourHoursAgo, now)
		logger.Info("Filtering slow logs",
			"start_time", twentyFourHoursAgo.Format(time.RFC3339),
			"end_time", now.Format(time.RFC3339),
			"patterns", timePatterns)

		// 4. 构建管道命令: grep [时间过滤] | mysqldumpslow
		grepCmd := exec.Command("grep", append([]string{"-E"}, timePatterns...)...)
		grepCmd.Args = append(grepCmd.Args, slowLogPath) // 追加日志文件路径

		mysqldumpslowCmd := exec.Command("mysqldumpslow", "-s", "c")
		mysqldumpslowCmd.Stdin, _ = grepCmd.StdoutPipe() // 连接管道
		var outputBuf bytes.Buffer
		mysqldumpslowCmd.Stdout = &outputBuf
		mysqldumpslowCmd.Stderr = &outputBuf // 合并标准错误到输出

		// 5. 执行管道命令
		if err := mysqldumpslowCmd.Start(); err != nil {
			logger.Error("Failed to start mysqldumpslow", "err", err)
			http.Error(w, "Failed to process slow logs", http.StatusInternalServerError)
			return
		}

		if err := grepCmd.Start(); err != nil {
			logger.Error("Failed to start grep", "err", err)
			http.Error(w, "Failed to filter logs", http.StatusInternalServerError)
			return
		}

		// 等待命令执行完成
		grepErr := grepCmd.Wait()
		mysqldumpErr := mysqldumpslowCmd.Wait()

		// 6. 处理命令输出
		output := outputBuf.String()
		logger.Debug("Command output", "length", len(output), "grep_err", grepErr, "mysqldump_err", mysqldumpErr)

		// 7. 解析结果（空结果返回空数组）
		results, err := parseSlowLogFile(logger, output)
		if err != nil || len(results) == 0 {
			logger.Info("No slow logs found in time range")
			results = []SlowLogResult{} // 确保返回空集合
		}
		if len(results) == 0 {
			logger.Info("No slow logs found in time range")
		}
		// 8. 返回JSON响应
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			logger.Error("Failed to encode response", "err", err)
		}
	}
}

// 生成24小时内的时间匹配模式（适配MySQL慢日志格式）
func generateTimePatterns(start, end time.Time) []string {
	var patterns []string

	// 处理跨天情况
	startDate := start.Format("060102") // MySQL日志常用格式: 年(后两位)月日
	endDate := end.Format("060102")

	if startDate == endDate {
		// 同一天: 匹配日期+时间范围
		patterns = append(patterns, fmt.Sprintf("# Time: %s", startDate))
	} else {
		// 跨天: 匹配起始日和结束日
		patterns = append(patterns, fmt.Sprintf("# Time: %s", startDate))
		patterns = append(patterns, fmt.Sprintf("# Time: %s", endDate))

		// 处理多天跨度（如果需要）
		current := start.Add(24 * time.Hour)
		for current.Format("060102") != endDate {
			patterns = append(patterns, fmt.Sprintf("# Time: %s", current.Format("060102")))
			current = current.Add(24 * time.Hour)
		}
	}

	return patterns
}
func getSlowLogPath(logger *slog.Logger) (string, error) {
	// 获取MySQL数据目录和慢查询日志文件名
	query := `
        SELECT 
            @@global.datadir AS datadir,
            @@global.slow_query_log_file AS logfile
    `

	// 使用已有的数据库连接配置
	cfg := c.GetConfig()
	cfgsection, ok := cfg.Sections["client"]
	if !ok {
		return "", fmt.Errorf("failed to get client section from config")
	}

	dsn, err := cfgsection.FormDSN("")
	if err != nil {
		return "", fmt.Errorf("failed to form DSN: %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", fmt.Errorf("failed to connect to database: %v", err)
	}
	defer db.Close()

	var dataDir, logFile string
	err = db.QueryRow(query).Scan(&dataDir, &logFile)
	if err != nil {
		return "", fmt.Errorf("failed to query slow log path: %v", err)
	}

	// 构建完整的慢日志路径
	// 注意: dataDir通常以/结尾，但logFile可能有或没有前导/
	fullPath := dataDir
	if !strings.HasSuffix(dataDir, "/") && !strings.HasPrefix(logFile, "/") {
		fullPath += "/"
	}
	fullPath += logFile

	// 规范化路径，处理可能的./或../
	fullPath = filepath.Clean(fullPath)

	return fullPath, nil
}
func checkFileAccess(path string) error {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	if fileInfo.IsDir() {
		return fmt.Errorf("path is a directory: %s", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open file: %w", err)
	}
	file.Close()
	return nil
}

func parseSlowLogFile(logger *slog.Logger, output string) ([]SlowLogResult, error) {
	var results []SlowLogResult
	lines := strings.Split(output, "\n")

	// 匹配mysqldumpslow输出的正则（示例格式：# Time: 240901 12:34:56）
	// mysqldumpslow输出格式示例：
	// Count: 10  Time=0.50s (5s)  Lock=0.00s (0s)  Rows=100.0 (1000), root[root]@localhost
	//   SELECT * FROM users WHERE name = ?
	re := regexp.MustCompile(`Count: (\d+)\s+Time=([\d.]+)s\s+\(([\d.]+)s\)\s+Lock=([\d.]+)s\s+\(([\d.]+)s\)\s+Rows=([\d.]+)\s+\(([\d.]+)\),\s+([^@]+)@([^\s]+)`)

	var currentResult *SlowLogResult

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 匹配统计行（包含Count、Time等信息）
		matches := re.FindStringSubmatch(line)
		if len(matches) == 10 {
			// 若有上一个未完成的结果，先加入列表
			if currentResult != nil {
				results = append(results, *currentResult)
			}

			// 解析统计信息
			count, _ := strconv.Atoi(matches[1])
			timeAvg, _ := strconv.ParseFloat(matches[2], 64)
			lockAvg, _ := strconv.ParseFloat(matches[4], 64)
			rowsSentAvg, _ := strconv.ParseFloat(matches[6], 64)
			rowsExaminedAvg, _ := strconv.ParseFloat(matches[7], 64) // 取总扫描行数估算平均值

			currentResult = &SlowLogResult{
				Count:        count,
				Time:         timeAvg,
				LockTime:     lockAvg,
				RowsSent:     rowsSentAvg,
				RowsExamined: rowsExaminedAvg,
				User:         matches[8],
				Host:         matches[9],
				Query:        "", // 后续行补充SQL语句
			}
		} else if currentResult != nil {
			// 补充SQL语句（可能跨多行）
			currentResult.Query += line + "\n"
		}
	}

	// 加入最后一个结果
	if currentResult != nil {
		currentResult.Query = strings.TrimSpace(currentResult.Query) // 去除多余换行
		results = append(results, *currentResult)
	}

	return results, nil
}
