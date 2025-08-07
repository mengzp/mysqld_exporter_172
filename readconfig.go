package main

import (
	"io/ioutil"
	"strings"
)

// MySQLConfig 表示 my.cnf 的结构化数据
type MySQLConfig struct {
	Sections map[string]map[string]string `json:"sections"`
	Raw      string                       `json:"raw"`
}

// readMySQLConfig 读取并解析 my.cnf 文件
func readMySQLConfig(path string) (*MySQLConfig, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	config := &MySQLConfig{
		Sections: make(map[string]map[string]string),
		Raw:      string(data),
	}

	currentSection := ""
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.Trim(line, "[]")
			config.Sections[currentSection] = make(map[string]string)
			continue
		}

		if currentSection != "" {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				config.Sections[currentSection][key] = value
			}
		}
	}

	return config, nil
}
